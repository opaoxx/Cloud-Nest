package service

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestUploadPartReceiptServiceList(t *testing.T) {
	actor := domain.Actor{UserID: 7, DeviceID: 31, SessionID: 41}
	const key = "users/7/objects/receipts-fixture"
	const uploadID = "fixture+A/="
	expiry := time.Date(2026, 11, 21, 12, 0, 0, 123456000, time.UTC)
	tail := uploadPartReceiptFixture(2, 1, "Tail")
	foreignFault := errors.New("fixture database failure")
	readFault := errors.New("private-storage-response")
	for _, test := range []struct {
		name        string
		firstError  error
		listError   error
		recheckErr  error
		change      func(*UploadPartSigningTarget)
		empty       bool
		invalidPart bool
		cancelAt    string
		wantError   error
		wantCalls   []string
	}{
		{name: "keep valid tail only", wantCalls: []string{"target", "storage", "target"}},
		{name: "empty result", empty: true, wantCalls: []string{"target", "storage", "target"}},
		{name: "valid renewed task with same session", change: func(target *UploadPartSigningTarget) {
			target.ExpiresAt = target.ExpiresAt.Add(time.Hour)
			target.LeaseVersion++
		}, wantCalls: []string{"target", "storage", "target"}},
		{name: "foreign or missing task", firstError: domain.ErrUploadTaskNotFound,
			wantError: domain.ErrUploadTaskNotFound, wantCalls: []string{"target"}},
		{name: "invalid stored receipt number", invalidPart: true,
			wantError: domain.ErrUploadStorageUnavailable, wantCalls: []string{"target", "storage"}},
		{name: "ordinary storage failure", listError: readFault,
			wantError: domain.ErrUploadStorageUnavailable, wantCalls: []string{"target", "storage"}},
		{name: "storage time limit", listError: context.DeadlineExceeded,
			wantError: domain.ErrUploadStorageUnavailable, wantCalls: []string{"target", "storage"}},
		{name: "storage session missing but task still readable", listError: domain.ErrUploadPartSessionMissing,
			wantError: domain.ErrUploadStorageUnavailable, wantCalls: []string{"target", "storage", "target"}},
		{name: "storage session missing and task left uploading", listError: domain.ErrUploadPartSessionMissing, recheckErr: domain.ErrUploadTaskNotReady,
			wantError: domain.ErrUploadTaskNotReady, wantCalls: []string{"target", "storage", "target"}},
		{name: "storage session missing and task binding changed", listError: domain.ErrUploadPartSessionMissing,
			change: func(target *UploadPartSigningTarget) { target.UploadID = "new-session" }, wantError: domain.ErrUploadTaskNotReady,
			wantCalls: []string{"target", "storage", "target"}},
		{name: "task expired while listing", recheckErr: domain.ErrUploadTaskNotReady,
			wantError: domain.ErrUploadTaskNotReady, wantCalls: []string{"target", "storage", "target"}},
		{name: "target changed", change: func(target *UploadPartSigningTarget) { target.StagedObjectID++; target.ObjectKey += "-new" },
			wantError: domain.ErrUploadTaskNotReady, wantCalls: []string{"target", "storage", "target"}},
		{name: "plan changed", change: func(target *UploadPartSigningTarget) { target.FileSizeBytes++ },
			wantError: domain.ErrUploadTaskNotReady, wantCalls: []string{"target", "storage", "target"}},
		{name: "database failure on recheck", recheckErr: foreignFault,
			wantError: foreignFault, wantCalls: []string{"target", "storage", "target"}},
		{name: "cancel before reading", cancelAt: "before", wantError: context.Canceled},
		{name: "cancel during storage read", cancelAt: "storage", listError: domain.ErrUploadPartSessionMissing,
			wantError: context.Canceled, wantCalls: []string{"target", "storage"}},
		{name: "cancel during final recheck", cancelAt: "recheck",
			wantError: context.Canceled, wantCalls: []string{"target", "storage", "target"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if test.cancelAt == "before" {
				cancel()
			}
			target := &UploadPartSigningTarget{TaskID: 19, UserID: 7, StagedObjectID: 23, LeaseVersion: 3,
				ObjectKey: key, UploadID: uploadID, FileSizeBytes: receiptFixturePartSize + 1,
				PartSizeBytes: receiptFixturePartSize, PartCount: 2, ExpiresAt: expiry}
			var calls []string
			service := NewUploadPartReceiptService(
				func(callContext context.Context, currentActor domain.Actor, taskID uint64) (*UploadPartSigningTarget, error) {
					calls = append(calls, "target")
					if callContext != ctx || currentActor != actor || taskID != 19 {
						t.Fatal("目标读取未使用相同认证身份和任务")
					}
					if len(calls) == 1 {
						return target, test.firstError
					}
					if test.cancelAt == "recheck" {
						cancel()
					}
					return target, test.recheckErr
				},
				func(callContext context.Context, objectKey, session string) ([]domain.UploadPartReceipt, error) {
					calls = append(calls, "storage")
					if callContext != ctx || objectKey != key || session != uploadID {
						t.Fatal("存储读取未使用首次核对的对象和会话")
					}
					if test.change != nil {
						test.change(target)
					}
					if test.cancelAt == "storage" {
						cancel()
					}
					if test.empty {
						return nil, test.listError
					}
					observed := []domain.UploadPartReceipt{uploadPartReceiptFixture(1, 1, "WrongLength"), tail}
					if test.invalidPart {
						observed[1].PartNumber = 3
					}
					return observed, test.listError
				},
			)
			result, err := service.List(ctx, actor, 19)
			if !errors.Is(err, test.wantError) || !reflect.DeepEqual(calls, test.wantCalls) {
				t.Fatal("回执读取顺序或错误类别不符", calls, err)
			}
			if test.wantError != nil {
				if result != nil {
					t.Fatal("失败后交付了已读取回执")
				}
				return
			}
			want := &domain.UploadPartReceiptList{PartSizeBytes: receiptFixturePartSize, PartCount: 2, Parts: []domain.UploadPartReceipt{tail}}
			if test.empty {
				want.Parts = []domain.UploadPartReceipt{}
			}
			if !reflect.DeepEqual(result, want) {
				t.Fatal("固定分片计划或可用回执不符")
			}
		})
	}
}
