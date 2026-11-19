package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestProcessUploadExpirationCandidateRetry(t *testing.T) {
	temporary := fmt.Errorf("收尾事务: %w", domain.ErrUploadExpirationRetryable)
	writeFailure := errors.New("fixture retry write failed")
	for _, scenario := range []struct {
		name          string
		attemptCount  uint32
		beginError    error
		completeError error
		writeError    error
		cancel        bool
		wantDelay     int
		wantError     error
	}{
		{name: "first retry waits one second", completeError: temporary, wantDelay: 1},
		{name: "retry delays increase", attemptCount: 4, completeError: temporary, wantDelay: 16},
		{name: "large attempt counts are capped", attemptCount: 63, completeError: temporary, wantDelay: 30},
		{name: "data conflict returns original error", completeError: domain.ErrStoredObjectInUse, wantError: domain.ErrStoredObjectInUse},
		{name: "failed begin has no committed task", beginError: temporary, wantError: temporary},
		{name: "cancellation before retry write", completeError: temporary, cancel: true, wantError: temporary},
		{name: "failed write is reported", completeError: temporary, writeError: writeFailure, wantDelay: 1, wantError: writeFailure},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 提供已提交的任务及本次收尾结果，记录重试写回的参数。
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			candidate := domain.UploadExpirationCandidate{
				TaskID: 19, UserID: 7, StagedObjectID: 23, UploadMode: "SINGLE", LeaseVersion: 3,
				ExpiresAt: time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC),
			}
			expiring := &domain.UploadTask{ID: candidate.TaskID, UserID: candidate.UserID, StagedObjectID: candidate.StagedObjectID,
				UploadMode: candidate.UploadMode, LeaseVersion: candidate.LeaseVersion, Status: domain.UploadStatusExpiring,
				ExpiresAt: candidate.ExpiresAt, AttemptCount: scenario.attemptCount}
			scheduled := *expiring
			scheduled.AttemptCount++
			scheduled.UpdatedAt = candidate.ExpiresAt.Add(time.Minute)
			nextAttempt := scheduled.UpdatedAt.Add(time.Duration(scenario.wantDelay) * time.Second)
			scheduled.NextAttemptAt = &nextAttempt
			retryCalls := 0
			processor := NewUploadExpirationCandidateProcessor(
				func(context.Context, domain.UploadExpirationCandidate) (*domain.UploadTask, error) {
					if scenario.beginError != nil {
						return nil, scenario.beginError
					}
					return expiring, nil
				},
				func(context.Context, *domain.UploadTask) (*domain.UploadTask, error) {
					if scenario.beginError != nil {
						t.Fatal("推进失败后仍执行了收尾")
					}
					if scenario.cancel {
						cancel()
					}
					return nil, scenario.completeError
				},
				func(writeContext context.Context, task *domain.UploadTask, delaySeconds int) (*domain.UploadTask, error) {
					retryCalls++
					if scenario.wantDelay == 0 || delaySeconds != scenario.wantDelay || task != expiring ||
						writeContext != ctx || writeContext.Err() != nil {
						t.Fatal("重试写回使用了错误的任务、时间或上下文", delaySeconds, task, writeContext.Err())
					}
					if scenario.writeError != nil {
						return nil, scenario.writeError
					}
					return &scheduled, nil
				},
			)

			// 2. 核对一次性写回及最终交给批次的任务或错误。
			result, err := processor.Process(ctx, candidate)
			if !errors.Is(err, scenario.wantError) || (scenario.wantDelay > 0 && retryCalls != 1) {
				t.Fatal("收尾后安排重试的结果不符", result, err, retryCalls)
			}
			if scenario.wantError == nil && result != &scheduled {
				t.Fatal("未返回写回提交的任务", result)
			}
			if scenario.wantError != nil && result != nil {
				t.Fatal("失败时返回了任务", result)
			}
		})
	}
}
