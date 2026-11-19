package domain

import (
	"errors"
	"math"
	"testing"
	"time"
)

func TestResolveSingleUploadCompletionStatus(t *testing.T) {
	// 1. 遍历两种模式和十种持久化状态，对照完成接口契约。
	for _, scenario := range []struct {
		status     UploadStatus
		wantStatus UploadStatus
		wantError  error
	}{
		{status: UploadStatusCreating, wantError: ErrUploadTaskNotReady},
		{status: UploadStatusUploading, wantError: ErrUploadTaskNotReady},
		{status: UploadStatusCompleting, wantError: ErrUploadTaskNotReady},
		{status: UploadStatusVerifying, wantStatus: UploadStatusVerifying},
		{status: UploadStatusCompleted, wantStatus: UploadStatusCompleted},
		{status: UploadStatusCancelling, wantError: ErrUploadTaskNotReady},
		{status: UploadStatusCancelled, wantError: ErrUploadTaskNotReady},
		{status: UploadStatusExpiring, wantError: ErrUploadTaskNotReady},
		{status: UploadStatusExpired, wantError: ErrUploadTaskNotReady},
		{status: UploadStatusFailed, wantError: ErrUploadTaskNotReady},
	} {
		for _, mode := range []string{"SINGLE", "MULTIPART"} {
			t.Run(mode+"/"+string(scenario.status), func(t *testing.T) {
				task := UploadTask{UploadMode: mode, Status: scenario.status}
				if scenario.status == UploadStatusCompleted {
					objectID, entryID := uint64(math.MaxUint64), uint64(math.MaxUint64)
					completedAt := time.Date(2026, 11, 20, 8, 0, 0, 123456000, time.UTC)
					task.PublishedObjectID, task.ResultEntryID, task.CompletedAt = &objectID, &entryID, &completedAt
					task.ExpiresAt = completedAt.Add(-time.Hour)
				}
				wantStatus, wantError := scenario.wantStatus, scenario.wantError
				if mode == "MULTIPART" {
					wantStatus, wantError = "", ErrUploadTaskNotReady
				}
				status, err := ResolveSingleUploadCompletionStatus(&task)
				if status != wantStatus || !errors.Is(err, wantError) {
					t.Fatalf("完成状态 = %q，错误 = %v，期望 %q、%v", status, err, wantStatus, wantError)
				}
			})
		}
	}
}

func TestResolveSingleUploadCompletionStatusInvalidData(t *testing.T) {
	// 1. 逐条构造阶段与结果字段矛盾或缺失的快照。
	objectID, entryID, zeroID := uint64(23), uint64(31), uint64(0)
	completedAt, zeroTime := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC), time.Time{}
	for _, scenario := range []struct {
		name string
		task *UploadTask
	}{
		{name: "nil task"},
		{name: "unknown mode", task: &UploadTask{UploadMode: "OTHER", Status: UploadStatusVerifying}},
		{name: "unknown status", task: &UploadTask{UploadMode: "SINGLE", Status: "UNKNOWN"}},
		{name: "verifying with publication object", task: &UploadTask{UploadMode: "SINGLE", Status: UploadStatusVerifying, PublishedObjectID: &objectID}},
		{name: "verifying with result entry", task: &UploadTask{UploadMode: "SINGLE", Status: UploadStatusVerifying, ResultEntryID: &entryID}},
		{name: "verifying with completion time", task: &UploadTask{UploadMode: "SINGLE", Status: UploadStatusVerifying, CompletedAt: &completedAt}},
		{name: "completed without object", task: &UploadTask{UploadMode: "SINGLE", Status: UploadStatusCompleted, ResultEntryID: &entryID, CompletedAt: &completedAt}},
		{name: "completed with zero object ID", task: &UploadTask{UploadMode: "SINGLE", Status: UploadStatusCompleted, PublishedObjectID: &zeroID, ResultEntryID: &entryID, CompletedAt: &completedAt}},
		{name: "completed without entry", task: &UploadTask{UploadMode: "SINGLE", Status: UploadStatusCompleted, PublishedObjectID: &objectID, CompletedAt: &completedAt}},
		{name: "completed with zero entry ID", task: &UploadTask{UploadMode: "SINGLE", Status: UploadStatusCompleted, PublishedObjectID: &objectID, ResultEntryID: &zeroID, CompletedAt: &completedAt}},
		{name: "completed without time", task: &UploadTask{UploadMode: "SINGLE", Status: UploadStatusCompleted, PublishedObjectID: &objectID, ResultEntryID: &entryID}},
		{name: "completed with zero time", task: &UploadTask{UploadMode: "SINGLE", Status: UploadStatusCompleted, PublishedObjectID: &objectID, ResultEntryID: &entryID, CompletedAt: &zeroTime}},
		{name: "completed with reservation", task: &UploadTask{UploadMode: "SINGLE", Status: UploadStatusCompleted, PublishedObjectID: &objectID, ResultEntryID: &entryID, CompletedAt: &completedAt, ReservedBytes: 1}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			status, err := ResolveSingleUploadCompletionStatus(scenario.task)
			if status != "" || !errors.Is(err, ErrInvalidUploadTaskData) {
				t.Fatalf("预期识别为任务数据异常，实际状态 = %q，错误 = %v", status, err)
			}
		})
	}
}

func TestResolveSingleUploadCompletionStatusUsesPersistedState(t *testing.T) {
	// 1. 模拟持租、等待重试以及已过原期限的校验中快照，重复确认只读取阶段。
	referenceTime := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	leaseUntil, retryAt := referenceTime.Add(time.Minute), referenceTime.Add(time.Hour)
	leaseOwner := "verification-worker"
	for _, scenario := range []struct {
		name   string
		update func(*UploadTask)
	}{
		{name: "active lease", update: func(task *UploadTask) { task.LeaseOwner, task.LeaseUntil = &leaseOwner, &leaseUntil }},
		{name: "scheduled retry", update: func(task *UploadTask) { task.NextAttemptAt = &retryAt }},
		{name: "deadline pending expiry worker", update: func(task *UploadTask) { task.ExpiresAt = referenceTime.Add(-time.Hour) }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			task := UploadTask{UploadMode: "SINGLE", Status: UploadStatusVerifying, FileSize: 9, ReservedBytes: 9, ExpiresAt: referenceTime.Add(time.Hour)}
			scenario.update(&task)
			before, originalOwner, originalLease, originalRetry := task, leaseOwner, leaseUntil, retryAt
			for range 2 {
				status, err := ResolveSingleUploadCompletionStatus(&task)
				if status != UploadStatusVerifying || err != nil {
					t.Fatalf("应保持校验中状态，实际状态 = %q，错误 = %v", status, err)
				}
			}
			if task != before || leaseOwner != originalOwner || leaseUntil != originalLease || retryAt != originalRetry {
				t.Fatal("完成确认改变了任务、租约或重试数据")
			}
		})
	}
}
