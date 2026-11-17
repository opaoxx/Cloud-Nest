package storage_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/internal/server/storage"
)

func TestPrepareUploadTempDirectoryResults(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "上传 文件", "nested", "uploads")
	settings := config.UploadTempConfig{Directory: directory}
	if err := storage.PrepareUploadTempDirectory(context.Background(), settings); err != nil {
		t.Fatal("创建并检查新目录失败", err)
	}
	info, err := os.Stat(directory)
	if err != nil || !info.IsDir() {
		t.Fatal("初始化后目标不是目录", err)
	}
	neighbor := filepath.Join(directory, "keep.txt")
	if err := os.WriteFile(neighbor, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := storage.PrepareUploadTempDirectory(context.Background(), settings); err != nil {
		t.Fatal("重复初始化失败", err)
	}
	entries, err := os.ReadDir(directory)
	content, readError := os.ReadFile(neighbor)
	if err != nil || len(entries) != 1 || entries[0].Name() != "keep.txt" || readError != nil || string(content) != "keep" {
		t.Fatal("探测文件未回收或已有内容被修改", err, readError)
	}
}

func TestPrepareUploadTempDirectoryFileFailures(t *testing.T) {
	for _, parentIsFile := range []bool{false, true} {
		name := "target is file"
		if parentIsFile {
			name = "parent is file"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			blocked := filepath.Join(root, "ordinary-file")
			if err := os.WriteFile(blocked, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			directory := blocked
			if parentIsFile {
				directory = filepath.Join(blocked, "uploads")
			}
			err := storage.PrepareUploadTempDirectory(context.Background(), config.UploadTempConfig{Directory: directory})
			var pathError *os.PathError
			content, readError := os.ReadFile(blocked)
			entries, listError := os.ReadDir(root)
			if err == nil || !errors.As(err, &pathError) || readError != nil || string(content) != "keep" || listError != nil || len(entries) != 1 {
				t.Fatal("文件冲突原因或既有内容保护错误", err, readError, listError)
			}
		})
	}
}

func TestPrepareUploadTempDirectoryInputs(t *testing.T) {
	root := t.TempDir()
	for _, fixture := range []struct {
		name, directory string
		ctx             context.Context
	}{
		{"nil context", filepath.Join(root, "unused"), nil},
		{"empty directory", "", context.Background()},
		{"relative directory", "upload-directory-fixture", context.Background()},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			if err := storage.PrepareUploadTempDirectory(fixture.ctx, config.UploadTempConfig{Directory: fixture.directory}); err == nil {
				t.Fatal("无效初始化参数被接受")
			}
		})
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatal("无效输入创建了目录", err)
	}
}

// cancelOnUploadProbe 在观察到本次探测文件时取消，验证创建后的真实清理路径。
type cancelOnUploadProbe struct {
	context.Context
	directory string
	cancel    context.CancelFunc
	probePath string
}

func (ctx *cancelOnUploadProbe) Err() error {
	entries, _ := os.ReadDir(ctx.directory)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".cloudnest-upload-probe-") {
			ctx.probePath = filepath.Join(ctx.directory, entry.Name())
			ctx.cancel()
		}
	}
	return ctx.Context.Err()
}

func TestPrepareUploadTempDirectoryCancellation(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		want := error(context.Canceled)
		if deadline {
			ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			want = context.DeadlineExceeded
		}
		directory := filepath.Join(t.TempDir(), "uncreated")
		err := storage.PrepareUploadTempDirectory(ctx, config.UploadTempConfig{Directory: directory})
		cancel()
		_, statError := os.Stat(directory)
		if !errors.Is(err, want) || !errors.Is(statError, os.ErrNotExist) {
			t.Fatal("提前取消仍创建目录或丢失原因", err, statError)
		}
	}
	directory := filepath.Join(t.TempDir(), "uploads")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observed := &cancelOnUploadProbe{Context: ctx, directory: directory, cancel: cancel}
	err := storage.PrepareUploadTempDirectory(observed, config.UploadTempConfig{Directory: directory})
	entries, listError := os.ReadDir(directory)
	if !errors.Is(err, context.Canceled) || observed.probePath == "" || listError != nil || len(entries) != 0 {
		t.Fatal("创建探测文件后取消未完成清理", err, listError)
	}
	if _, err := os.Stat(observed.probePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("取消后探测文件仍存在", err)
	}
}

func TestPrepareUploadTempDirectoryConcurrent(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "shared", "uploads")
	var callers sync.WaitGroup
	for range 8 {
		callers.Go(func() {
			if err := storage.PrepareUploadTempDirectory(context.Background(), config.UploadTempConfig{Directory: directory}); err != nil {
				t.Error("并发目录初始化失败", err)
			}
		})
	}
	callers.Wait()
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatal("并发探测留下临时文件", err)
	}
}

func TestPrepareUploadTempDirectoryReceiveFlow(t *testing.T) {
	t.Setenv("CLOUDNEST_TEMP_DIR", filepath.Join(t.TempDir(), "configured", "uploads"))
	settings, err := config.LoadUploadTempConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.PrepareUploadTempDirectory(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	space := service.NewUploadTempSpace()
	receiver, err := service.NewUploadContentFileService(settings.Directory, space)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	err = receiver.WithFile(context.Background(), bytes.NewBufferString("abc"), 3, nil, func(content io.ReadSeeker, digest domain.UploadContentDigest) error {
		calls++
		payload, err := io.ReadAll(content)
		if err != nil || string(payload) != "abc" || digest.SizeBytes != 3 {
			t.Fatal("初始化目录后的实际接收错误", err)
		}
		return nil
	})
	entries, listError := os.ReadDir(settings.Directory)
	if err != nil || calls != 1 || listError != nil || len(entries) != 0 {
		t.Fatal("配置到目录再到接收的结果错误", err, listError)
	}
	release, err := space.Reserve(context.Background(), 128*1024*1024)
	if err != nil {
		t.Fatal("接收后预算无法完整复用", err)
	}
	release()
}
