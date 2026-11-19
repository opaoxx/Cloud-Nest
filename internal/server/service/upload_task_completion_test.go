package service

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestUploadTaskServiceConfirmCompletionRefreshesSnapshot(t *testing.T) {
	// 1. 模拟同账号的其他设备先读到等待重试，之后读到已保存的发布结果。
	actor, completed := uploadTaskServiceFixture()
	actor.DeviceID = 15
	completed.UploadMode = "SINGLE"
	completed.StorageUploadID, completed.PartSize, completed.PartCount = nil, nil, nil
	completed.LeaseOwner, completed.LeaseUntil, completed.NextAttemptAt = nil, nil, nil
	verifying := completed
	verifying.Status, verifying.ReservedBytes = domain.UploadStatusVerifying, verifying.FileSize
	verifying.PublishedObjectID, verifying.ResolvedName, verifying.ResultEntryID, verifying.CompletedAt = nil, nil, nil, nil
	retryAt := verifying.UpdatedAt.Add(30 * time.Second)
	verifying.NextAttemptAt = &retryAt
	source := &verifying
	ctx := t.Context()
	reads := 0
	tasks := NewUploadTaskService(func(received context.Context, userID, taskID uint64) (*domain.UploadTask, error) {
		reads++
		if received != ctx || userID != actor.UserID || taskID != completed.ID {
			t.Fatal("完成确认未沿用请求上下文、用户归属或任务 ID", userID, taskID)
		}
		return source, nil
	})

	// 2. 第一次交付校验快照，第二次读取已发布状态及原目录项 ID。
	first, err := tasks.ConfirmCompletion(ctx, actor, completed.ID)
	if err != nil || !reflect.DeepEqual(first, &verifying) || first == &verifying || first.NextAttemptAt == &retryAt {
		t.Fatal("未交付独立的校验中快照", first, err)
	}
	*first.NextAttemptAt = time.Time{}
	source = &completed
	second, err := tasks.ConfirmCompletion(ctx, actor, completed.ID)
	if err != nil || !reflect.DeepEqual(second, &completed) || second == &completed || second.ResultEntryID == completed.ResultEntryID || retryAt.IsZero() {
		t.Fatal("未按新读到的发布结果交付，或修改影响了来源", second, err)
	}

	// 3. 调用方修改本次副本后，重复确认仍能读到同一发布结果。
	*second.ResultEntryID = 999
	repeated, err := tasks.ConfirmCompletion(ctx, actor, completed.ID)
	if err != nil || !reflect.DeepEqual(repeated, &completed) || *repeated.ResultEntryID != 31 || repeated.DeviceID != 11 || reads != 3 {
		t.Fatal("重复确认未读取独立结果，或改变了来源设备", repeated, reads, err)
	}
}

func TestUploadTaskServiceConfirmCompletionFailures(t *testing.T) {
	queryFailure := errors.New("fixture task lookup failure")
	for _, scenario := range []struct {
		name           string
		changeTask     func(*domain.UploadTask)
		queryError     error
		cancelAfterGet bool
		wantError      error
	}{
		{name: "task not found", queryError: domain.ErrUploadTaskNotFound, wantError: domain.ErrUploadTaskNotFound},
		{name: "query failed", queryError: queryFailure, wantError: queryFailure},
		{name: "wrong target", changeTask: func(task *domain.UploadTask) { task.ID++ }, wantError: domain.ErrInvalidUploadTaskData},
		{name: "wrong owner", changeTask: func(task *domain.UploadTask) { task.UserID++ }, wantError: domain.ErrInvalidUploadTaskData},
		{name: "waiting for content", changeTask: func(task *domain.UploadTask) { task.Status = domain.UploadStatusUploading }, wantError: domain.ErrUploadTaskNotReady},
		{name: "incomplete publication", changeTask: func(task *domain.UploadTask) { task.ResultEntryID = nil }, wantError: domain.ErrInvalidUploadTaskData},
		{name: "canceled after lookup", cancelAfterGet: true, wantError: context.Canceled},
		{name: "canceled with query failure", cancelAfterGet: true, queryError: queryFailure, wantError: context.Canceled},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 让真实查询服务处理错误或异常快照，再核对完成确认的失败结果。
			actor, task := uploadTaskServiceFixture()
			task.UploadMode = "SINGLE"
			task.StorageUploadID, task.PartSize, task.PartCount = nil, nil, nil
			taskID := task.ID
			if scenario.changeTask != nil {
				scenario.changeTask(&task)
			}
			ctx, stop := context.WithCancel(t.Context())
			defer stop()
			reads := 0
			tasks := NewUploadTaskService(func(context.Context, uint64, uint64) (*domain.UploadTask, error) {
				reads++
				if scenario.cancelAfterGet {
					stop()
				}
				return &task, scenario.queryError
			})
			result, err := tasks.ConfirmCompletion(ctx, actor, taskID)
			if result != nil || reads != 1 || !errors.Is(err, scenario.wantError) {
				t.Fatal("完成确认返回了快照或丢失错误原因", result, reads, err)
			}
		})
	}
}
