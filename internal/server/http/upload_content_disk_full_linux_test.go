package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestUploadContentTempDiskFullIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_SINGLE_UPLOAD_DISK_FULL_TEST") != "1" {
		t.Skip("仅在本轮专用的 Linux tmpfs 容器内运行")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	// 1. 只使用容器内 1 MiB 专用 tmpfs，保持正常的接收预算和正式上传路由。
	const directory = "/cloudnest-upload-proof"
	const diskBytes = 1 << 20
	const tmpfsMagic = 0x01021994
	var filesystem syscall.Statfs_t
	if err := syscall.Statfs(directory, &filesystem); err != nil || filesystem.Type != tmpfsMagic ||
		filesystem.Blocks*uint64(filesystem.Bsize) != diskBytes || filesystem.Bavail != filesystem.Blocks {
		t.Fatal("缺少全空且容量为 1 MiB 的专用 tmpfs", err)
	}
	space := service.NewUploadTempSpace()
	files, err := service.NewUploadContentFileService(directory, space)
	if err != nil {
		t.Fatal("创建隔离暂存服务失败", err)
	}
	payload := bytes.Repeat([]byte{0x37}, 2*diskBytes)
	now := time.Now().UTC()
	actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
	task := domain.UploadTask{
		ID: 19, UserID: actor.UserID, DeviceID: actor.DeviceID, StagedObjectID: 23, TargetParentID: 13,
		UploadMode: "SINGLE", Status: domain.UploadStatusUploading, FileSize: uint64(len(payload)),
		ReservedBytes: uint64(len(payload)), UpdatedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	receiver := service.NewUploadContentReceiveService(files, "local_minio", "fixture-bucket",
		func(context.Context, domain.Actor, uint64) (*domain.UploadContentPreparation, error) {
			leased := task
			leaseOwner, leaseUntil := "fixture-receive", now.Add(time.Minute)
			leased.LeaseOwner, leased.LeaseUntil, leased.LeaseVersion = &leaseOwner, &leaseUntil, 1
			object := &domain.StoredObject{ID: leased.StagedObjectID, OwnerUserID: leased.UserID,
				Status: domain.StoredObjectStatusStaging, FileSize: leased.FileSize,
				Provider: "local_minio", Bucket: "fixture-bucket", ObjectKey: "fixture-key"}
			return &domain.UploadContentPreparation{Task: &leased, Object: object}, nil
		},
		func(context.Context, string, io.ReadSeeker, uint64) (*domain.UploadObjectResult, error) {
			t.Error("暂存写入失败后仍调用对象存储")
			return nil, domain.ErrUploadStorageUnavailable
		},
		func(context.Context, domain.Actor, func(context.Context, service.UploadContentReceiveTransaction) error) error {
			t.Error("暂存写入失败后仍进入内容接收事务")
			return domain.ErrUploadTaskBusy
		})
	receiveErrors := make(chan error, 1)
	router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
	RegisterUploadContentRoute(router, func(_ context.Context, token string) (*domain.Actor, error) {
		if token != "fixture-access" {
			return nil, domain.ErrInvalidAccessToken
		}
		return &actor, nil
	}, func(_ context.Context, current domain.Actor, taskID uint64) (*domain.UploadTask, error) {
		if current != actor || taskID != task.ID {
			return nil, domain.ErrUploadTaskNotFound
		}
		return &task, nil
	}, func(ctx context.Context, actor domain.Actor, task *domain.UploadTask, source io.Reader) (*domain.UploadTask, error) {
		result, err := receiver.Receive(ctx, actor, task, source)
		receiveErrors <- err
		return result, err
	})
	server := httptest.NewServer(router)
	defer server.Close()

	// 2. 发完整正文，核对底层确实在写专用临时文件时报告 ENOSPC。
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, server.URL+"/api/v1/uploads/19/content", bytes.NewReader(payload))
	if err != nil {
		t.Fatal("构造隔离上传请求失败", err)
	}
	request.Header.Set("Authorization", "Bearer fixture-access")
	request.Header.Set("Content-Type", "application/octet-stream")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal("隔离上传请求失败", err)
	}
	defer response.Body.Close()
	var receiveError error
	select {
	case receiveError = <-receiveErrors:
	case <-ctx.Done():
		t.Fatal("正式路由未进入内容接收", ctx.Err())
	}
	var writeFailure *os.PathError
	if !errors.Is(receiveError, syscall.ENOSPC) || !errors.As(receiveError, &writeFailure) ||
		writeFailure.Op != "write" || filepath.Dir(writeFailure.Path) != directory {
		t.Fatal("没有捕获专用暂存盘的实际写入错误", receiveError)
	}

	// 3. 确认临时文件已删除，完整暂存预算可再次申请，公开错误只含固定提示。
	if entries, err := os.ReadDir(directory); err != nil || len(entries) != 0 {
		t.Fatal("暂存失败后仍有临时文件", len(entries), err)
	}
	release, err := space.Reserve(ctx, 128<<20)
	if err != nil {
		t.Fatal("暂存文件清理后预算未全额归还", err)
	}
	release()
	var rejected dto.ErrorResponse
	if err := json.NewDecoder(response.Body).Decode(&rejected); err != nil {
		t.Fatal("磁盘满错误响应格式不符", err)
	}
	t.Logf("临时盘 %d B，声明 %d B，写入收到 ENOSPC，HTTP %d %s，临时文件 0，已验证可重新申请全部 128 MiB 预算",
		diskBytes, len(payload), response.StatusCode, rejected.Error.Code)
	if response.StatusCode != http.StatusInsufficientStorage || rejected.Error.Code != "TEMP_SPACE_EXHAUSTED" ||
		rejected.Error.Message != "上传临时空间不足，请稍后重试" || rejected.Error.Details != nil ||
		rejected.RequestID == "" || rejected.RequestID != response.Header.Get("X-Request-ID") {
		t.Fatal("磁盘满错误响应与暂存空间契约不符", rejected)
	}
}
