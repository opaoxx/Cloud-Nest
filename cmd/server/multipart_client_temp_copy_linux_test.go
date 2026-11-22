package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestMultipartClientTempCopyDiskFull(t *testing.T) {
	if os.Getenv("CLOUDNEST_CLIENT_TEMP_COPY_TEST") != "1" {
		t.Skip("仅在本轮专用的 Linux tmpfs 容器内运行")
	}
	const directory = "/cloudnest-client-copy-proof"
	const diskBytes = 1 << 20
	const tmpfsMagic = 0x01021994
	var filesystem syscall.Statfs_t
	if err := syscall.Statfs(directory, &filesystem); err != nil || filesystem.Type != tmpfsMagic ||
		filesystem.Blocks*uint64(filesystem.Bsize) != diskBytes || filesystem.Bavail != filesystem.Blocks {
		t.Fatal("缺少全空且容量为 1 MiB 的专用 tmpfs", err)
	}

	control := bytes.Repeat([]byte{0x57}, 64<<10)
	var creates, writes, unexpected atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/uploads":
			creates.Add(1)
			response.WriteHeader(http.StatusCreated)
		case request.Method == http.MethodPut && request.URL.Path == "/api/v1/uploads/1/content":
			writes.Add(1)
			body, err := io.ReadAll(io.LimitReader(request.Body, int64(len(control))+1))
			if err != nil || !bytes.Equal(body, control) {
				t.Error("回环探针收到的副本内容不符", err)
				response.WriteHeader(http.StatusBadRequest)
				return
			}
			response.WriteHeader(http.StatusOK)
		default:
			unexpected.Add(1)
			response.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	send := func(copyFile *os.File) error {
		// 回环探针只记录创建和写入，便于核对客户端是否越过准备阶段。
		createBody := strings.NewReader(fmt.Sprintf(`{"size_bytes":%d}`, len(control)))
		for _, request := range []struct {
			method, path string
			body         io.Reader
			status       int
		}{
			{http.MethodPost, "/api/v1/uploads", createBody, http.StatusCreated},
			{http.MethodPut, "/api/v1/uploads/1/content", io.NopCloser(copyFile), http.StatusOK},
		} {
			outbound, err := http.NewRequestWithContext(t.Context(), request.method, server.URL+request.path, request.body)
			if err != nil {
				return err
			}
			reply, err := server.Client().Do(outbound)
			if err != nil {
				return err
			}
			reply.Body.Close()
			if reply.StatusCode != request.status {
				return fmt.Errorf("%s %s 返回 %d，期望 %d", request.method, request.path, reply.StatusCode, request.status)
			}
		}
		return nil
	}

	// 1. 以真实写入制造 ENOSPC，并在成功对照前独立检查所有请求计数。
	payload := make([]byte, domain.MaxSingleUploadSizeBytes+1)
	result, err := prepareAndSendTestClientCopy(t, directory, bytes.NewBuffer(payload), send)
	var writeFailure *os.PathError
	if !errors.Is(err, syscall.ENOSPC) || !errors.As(err, &writeFailure) ||
		writeFailure.Op != "write" || filepath.Dir(writeFailure.Path) != directory {
		t.Fatal("未捕获专用临时盘的实际写入错误", err)
	}
	if result.copied != diskBytes || result.status != "PAUSED" || result.reason != "客户端临时副本空间不足" {
		t.Fatal("临时副本写满后未以明确原因暂停", result)
	}
	if creates.Load()+writes.Load()+unexpected.Load() != 0 {
		t.Fatal("准备失败后回环探针收到了请求", creates.Load(), writes.Load(), unexpected.Load())
	}
	if entries, err := os.ReadDir(directory); err != nil || len(entries) != 0 {
		t.Fatal("暂停后仍有临时副本", len(entries), err)
	}
	t.Logf("临时盘 %d B，合成源 %d B，写满后 %s（%s，ENOSPC），创建 0、上传 0、残留副本 0",
		diskBytes, len(payload), result.status, result.reason)

	// 2. 在同一目录用可写完的小样本走同一条路径，验证探针能看到请求及副本正文。
	result, err = prepareAndSendTestClientCopy(t, directory, bytes.NewBuffer(control), send)
	if err != nil || result.copied != int64(len(control)) || result.status != "SENT" || result.reason != "" ||
		creates.Load() != 1 || writes.Load() != 1 || unexpected.Load() != 0 {
		t.Fatal("成功对照未完整通过", result, creates.Load(), writes.Load(), unexpected.Load(), err)
	}
	if entries, err := os.ReadDir(directory); err != nil || len(entries) != 0 {
		t.Fatal("对照结束后仍有临时副本", len(entries), err)
	}
	t.Logf("成功对照 %d B，创建 1、上传 1，探针收到逐字节一致的副本，残留副本 0", result.copied)
}

type testClientCopyResult struct {
	copied int64
	status string
	reason string
}

// prepareAndSendTestClientCopy 模拟需要先落盘的客户端输入，供隔离验收使用。
func prepareAndSendTestClientCopy(t *testing.T, directory string, source io.Reader, send func(*os.File) error) (testClientCopyResult, error) {
	t.Helper()
	var result testClientCopyResult
	// 1. 把合成输入写入专用临时文件。
	copyFile, err := os.CreateTemp(directory, "upload-source-*")
	if err != nil {
		return result, err
	}
	defer func() {
		if err := copyFile.Close(); err != nil {
			t.Error("关闭专用临时副本失败", err)
		}
		if err := os.Remove(copyFile.Name()); err != nil {
			t.Error("清理专用临时副本失败", err)
		}
	}()
	result.copied, err = io.Copy(copyFile, source)

	// 2. 写满时返回暂停原因，并清理未写完的副本。
	if err != nil {
		if errors.Is(err, syscall.ENOSPC) {
			result.status = "PAUSED"
			result.reason = "客户端临时副本空间不足"
		}
		return result, err
	}

	// 3. 完整写入后才进入创建和上传路径。
	if _, err := copyFile.Seek(0, io.SeekStart); err != nil {
		return result, err
	}
	if err := send(copyFile); err != nil {
		return result, err
	}
	result.status = "SENT"
	return result, nil
}
