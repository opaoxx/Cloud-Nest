package service

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestResolveMultipartUploadCompletionStatusStages(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 123456000, time.UTC)
	for _, test := range []struct {
		status     domain.UploadStatus
		wantStatus domain.UploadStatus
		wantError  error
	}{
		{status: domain.UploadStatusCreating, wantError: domain.ErrUploadTaskNotReady},
		{status: domain.UploadStatusUploading, wantStatus: domain.UploadStatusUploading},
		{status: domain.UploadStatusCompleting, wantStatus: domain.UploadStatusCompleting},
		{status: domain.UploadStatusVerifying, wantStatus: domain.UploadStatusVerifying},
		{status: domain.UploadStatusCompleted, wantStatus: domain.UploadStatusCompleted},
		{status: domain.UploadStatusCancelling, wantError: domain.ErrUploadTaskNotReady},
		{status: domain.UploadStatusCancelled, wantError: domain.ErrUploadTaskNotReady},
		{status: domain.UploadStatusExpiring, wantError: domain.ErrUploadTaskNotReady},
		{status: domain.UploadStatusExpired, wantError: domain.ErrUploadTaskNotReady},
		{status: domain.UploadStatusFailed, wantError: domain.ErrUploadTaskNotReady},
	} {
		t.Run(string(test.status), func(t *testing.T) {
			task := uploadPartSigningTaskFixture(now.Add(time.Hour))
			task.Status = test.status
			// 1. 已受理的任务仍能交付当前状态，包括任务原期限已过和后台持租的情况。
			switch task.Status {
			case domain.UploadStatusCompleting, domain.UploadStatusVerifying:
				task.ExpiresAt = now.Add(-time.Minute)
				task.LeaseOwner, task.LeaseUntil = new("fixture-worker"), new(now.Add(time.Minute))
				task.NextAttemptAt = new(now.Add(time.Minute))
				if task.Status == domain.UploadStatusVerifying {
					task.StorageUploadID = nil
				}
			case domain.UploadStatusCompleted:
				task.ExpiresAt = now.Add(-time.Hour)
				task.StorageUploadID = nil
				task.ReservedBytes = 0
				task.PublishedObjectID, task.ResultEntryID, task.CompletedAt = new(uint64(23)), new(uint64(41)), new(now.Add(-time.Minute))
			}

			// 2. 两次判定使用同一快照，状态、发布结果和租约值保持一致。
			before := copyUploadTask(task)
			for range 2 {
				status, err := ResolveMultipartUploadCompletionStatus(task, 7, 19, now)
				if status != test.wantStatus || !errors.Is(err, test.wantError) || !reflect.DeepEqual(task, before) {
					t.Fatalf("阶段判定或只读保证不符，得到 %q、%v，预期 %q、%v", status, err, test.wantStatus, test.wantError)
				}
			}
		})
	}
}

func TestResolveMultipartUploadCompletionStatusNewRound(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 123456000, time.UTC)
	for _, test := range []struct {
		name   string
		update func(*domain.UploadTask)
		want   error
	}{
		{"one microsecond before expiry", func(task *domain.UploadTask) { task.ExpiresAt = now.Add(time.Microsecond) }, nil},
		{"at expiry", func(task *domain.UploadTask) { task.ExpiresAt = now }, domain.ErrUploadTaskNotReady},
		{"active lease", func(task *domain.UploadTask) {
			task.LeaseOwner, task.LeaseUntil = new("fixture-worker"), new(now.Add(time.Microsecond))
		}, domain.ErrUploadTaskBusy},
		{"lease reaches deadline", func(task *domain.UploadTask) {
			task.LeaseOwner, task.LeaseUntil = new("fixture-worker"), &now
		}, nil},
		{"missing session", func(task *domain.UploadTask) { task.StorageUploadID = nil }, domain.ErrUploadTaskNotReady},
		{"invalid plan", func(task *domain.UploadTask) { task.PartCount = new(uint32(3)) }, domain.ErrInvalidUploadTaskData},
		{"lease has no owner", func(task *domain.UploadTask) { task.LeaseUntil = new(now.Add(time.Minute)) }, domain.ErrInvalidUploadTaskData},
	} {
		t.Run(test.name, func(t *testing.T) {
			task := uploadPartSigningTaskFixture(now.Add(time.Hour))
			test.update(task)
			wantStatus := domain.UploadStatusUploading
			if test.want != nil {
				wantStatus = ""
			}
			status, err := ResolveMultipartUploadCompletionStatus(task, 7, 19, now)
			if status != wantStatus || !errors.Is(err, test.want) {
				t.Fatalf("受理窗口判断不符，得到 %q、%v，预期 %q、%v", status, err, wantStatus, test.want)
			}
		})
	}
}

func TestResolveMultipartUploadCompletionStatusRejectsInvalidSnapshots(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 123456000, time.UTC)
	for _, test := range []struct {
		name   string
		update func(*domain.UploadTask)
		want   error
	}{
		{"different account", func(task *domain.UploadTask) { task.UserID++ }, domain.ErrInvalidUploadTaskData},
		{"different task", func(task *domain.UploadTask) { task.ID++ }, domain.ErrInvalidUploadTaskData},
		{"single mode", func(task *domain.UploadTask) { task.UploadMode = "SINGLE" }, domain.ErrUploadTaskNotReady},
		{"unknown mode", func(task *domain.UploadTask) { task.UploadMode = "OTHER" }, domain.ErrInvalidUploadTaskData},
		{"unknown status", func(task *domain.UploadTask) { task.Status = "OTHER" }, domain.ErrInvalidUploadTaskData},
		{"completing already has result", func(task *domain.UploadTask) {
			task.Status, task.ResultEntryID = domain.UploadStatusCompleting, new(uint64(41))
		}, domain.ErrInvalidUploadTaskData},
		{"verifying already has completion time", func(task *domain.UploadTask) {
			task.Status, task.CompletedAt = domain.UploadStatusVerifying, &now
		}, domain.ErrInvalidUploadTaskData},
		{"completed without result", func(task *domain.UploadTask) { task.ResultEntryID = nil }, domain.ErrInvalidUploadTaskData},
		{"completed with zero object ID", func(task *domain.UploadTask) { task.PublishedObjectID = new(uint64(0)) }, domain.ErrInvalidUploadTaskData},
		{"completed with zero completion time", func(task *domain.UploadTask) { task.CompletedAt = new(time.Time{}) }, domain.ErrInvalidUploadTaskData},
		{"completed with reservation", func(task *domain.UploadTask) { task.ReservedBytes = 1 }, domain.ErrInvalidUploadTaskData},
	} {
		t.Run(test.name, func(t *testing.T) {
			task := uploadPartSigningTaskFixture(now.Add(time.Hour))
			task.Status = domain.UploadStatusCompleted
			task.PublishedObjectID, task.ResultEntryID, task.CompletedAt = new(uint64(23)), new(uint64(41)), &now
			task.ReservedBytes = 0
			test.update(task)
			status, err := ResolveMultipartUploadCompletionStatus(task, 7, 19, now)
			if status != "" || !errors.Is(err, test.want) {
				t.Fatalf("预期拒绝错误快照，得到 %q、%v，预期错误 %v", status, err, test.want)
			}
		})
	}
}
