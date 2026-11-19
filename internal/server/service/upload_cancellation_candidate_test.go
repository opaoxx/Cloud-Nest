package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestProcessUploadCancellationCandidate(t *testing.T) {
	candidate := domain.UploadCancellationCandidate{TaskID: 19, UserID: 7, StagedObjectID: 23, LeaseVersion: math.MaxUint64}
	for _, scenario := range []struct {
		name          string
		changeTask    func(*domain.UploadTask)
		missingRecord bool
		findError     error
		completeError error
		stopAfterRead bool
		wantComplete  bool
		wantError     error
	}{
		{name: "complete current cancellation", wantComplete: true},
		{name: "replay already cancelled", changeTask: func(task *domain.UploadTask) { task.Status = domain.UploadStatusCancelled }, wantComplete: true},
		{name: "missing task", findError: domain.ErrUploadTaskNotFound, wantError: domain.ErrUploadTaskNotFound},
		{name: "nil record", missingRecord: true, wantError: domain.ErrInvalidUploadTaskData},
		{name: "wrong task ID", changeTask: func(task *domain.UploadTask) { task.ID++ }, wantError: domain.ErrInvalidUploadTaskData},
		{name: "wrong user ID", changeTask: func(task *domain.UploadTask) { task.UserID++ }, wantError: domain.ErrInvalidUploadTaskData},
		{name: "changed upload mode", changeTask: func(task *domain.UploadTask) { task.UploadMode = "MULTIPART" }, wantError: domain.ErrUploadTaskNotReady},
		{name: "changed status", changeTask: func(task *domain.UploadTask) { task.Status = domain.UploadStatusExpiring }, wantError: domain.ErrUploadTaskNotReady},
		{name: "changed staged object", changeTask: func(task *domain.UploadTask) { task.StagedObjectID++ }, wantError: domain.ErrUploadTaskBusy},
		{name: "changed lease version", changeTask: func(task *domain.UploadTask) { task.LeaseVersion-- }, wantError: domain.ErrUploadTaskBusy},
		{name: "stopped after read", stopAfterRead: true, wantError: context.Canceled},
		{name: "cleanup fails", wantComplete: true, completeError: domain.ErrStoredObjectInUse, wantError: domain.ErrStoredObjectInUse},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 准备候选当前任务和收尾结果，记录读取与写回的次数。
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			task := &domain.UploadTask{ID: candidate.TaskID, UserID: candidate.UserID, StagedObjectID: candidate.StagedObjectID,
				LeaseVersion: candidate.LeaseVersion, UploadMode: "SINGLE", Status: domain.UploadStatusCancelling}
			if scenario.changeTask != nil {
				scenario.changeTask(task)
			}
			result := *task
			result.Status = domain.UploadStatusCancelled
			readCalls, completeCalls := 0, 0
			processor := NewUploadCancellationCandidateProcessor(
				func(readContext context.Context, userID, taskID uint64) (*domain.UploadTask, error) {
					readCalls++
					if readContext != ctx || userID != candidate.UserID || taskID != candidate.TaskID {
						t.Fatal("重读未使用候选的用户和任务范围", userID, taskID)
					}
					if scenario.stopAfterRead {
						cancel()
					}
					if scenario.missingRecord || scenario.findError != nil {
						return nil, scenario.findError
					}
					return task, nil
				},
				func(writeContext context.Context, current *domain.UploadTask) (*domain.UploadTask, error) {
					completeCalls++
					if !scenario.wantComplete || current != task || writeContext != ctx || writeContext.Err() != nil {
						t.Fatal("未按当前任务和上下文调用收尾", current, writeContext.Err())
					}
					if scenario.completeError != nil {
						return nil, scenario.completeError
					}
					return &result, nil
				},
				func(context.Context, *domain.UploadTask, int) (*domain.UploadTask, error) {
					t.Fatal("无取消重试标记时仍安排了重试")
					return nil, nil
				},
			)

			// 2. 核对成功交付已提交任务，或在预期位置返回原错误。
			finished, err := processor.Process(ctx, candidate)
			if readCalls != 1 || (scenario.wantComplete && completeCalls != 1) || !errors.Is(err, scenario.wantError) {
				t.Fatal("候选处理结果或调用次数不符", finished, err, readCalls, completeCalls)
			}
			if (err == nil && finished != &result) || (err != nil && finished != nil) {
				t.Fatal("候选处理返回了错误的任务", finished)
			}
		})
	}
}

func TestProcessUploadCancellationCandidateRejectsBeforeLookup(t *testing.T) {
	// 1. 无效候选或已取消的上下文都应在查库前结束。
	processor := NewUploadCancellationCandidateProcessor(
		func(context.Context, uint64, uint64) (*domain.UploadTask, error) {
			t.Fatal("无效或已取消的处理仍查询了任务")
			return nil, nil
		},
		func(context.Context, *domain.UploadTask) (*domain.UploadTask, error) {
			t.Fatal("无效或已取消的处理仍调用了收尾")
			return nil, nil
		},
		func(context.Context, *domain.UploadTask, int) (*domain.UploadTask, error) {
			t.Fatal("无效或已取消的处理仍安排了重试")
			return nil, nil
		},
	)
	for _, candidate := range []domain.UploadCancellationCandidate{
		{UserID: 7, StagedObjectID: 23}, {TaskID: 19, StagedObjectID: 23}, {TaskID: 19, UserID: 7},
	} {
		if task, err := processor.Process(t.Context(), candidate); task != nil || !errors.Is(err, domain.ErrInvalidUploadTaskData) {
			t.Fatal("无效候选未被拒绝", candidate, task, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	candidate := domain.UploadCancellationCandidate{TaskID: 19, UserID: 7, StagedObjectID: 23}
	if task, err := processor.Process(ctx, candidate); task != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("已取消的调用未结束", task, err)
	}
}

func TestProcessUploadCancellationCandidateRetry(t *testing.T) {
	candidate := domain.UploadCancellationCandidate{TaskID: 19, UserID: 7, StagedObjectID: 23, LeaseVersion: 3}
	temporary := fmt.Errorf("收尾事务: %w", domain.ErrUploadCancellationRetryable)
	scheduleFailure := errors.New("fixture schedule retry failed")
	for _, scenario := range []struct {
		name          string
		attemptCount  uint32
		stop          bool
		terminal      bool
		completeError error
		scheduleError error
		wantDelay     int
		wantError     error
	}{
		{name: "first retry", completeError: temporary, wantDelay: 1},
		{name: "existing attempts", attemptCount: 4, completeError: temporary, wantDelay: 16},
		{name: "delay cap", attemptCount: math.MaxUint32 - 1, completeError: temporary, wantDelay: 30},
		{name: "writeback fails", completeError: temporary, scheduleError: scheduleFailure, wantDelay: 1, wantError: scheduleFailure},
		{name: "worker stopping", stop: true, completeError: temporary, wantError: temporary},
		{name: "terminal replay failed", terminal: true, completeError: temporary, wantError: temporary},
		{name: "different marker", completeError: domain.ErrUploadExpirationRetryable, wantError: domain.ErrUploadExpirationRetryable},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 让收尾返回指定错误，用当前任务保留重试次数。
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			task := &domain.UploadTask{ID: candidate.TaskID, UserID: candidate.UserID, StagedObjectID: candidate.StagedObjectID,
				LeaseVersion: candidate.LeaseVersion, UploadMode: "SINGLE", Status: domain.UploadStatusCancelling, AttemptCount: scenario.attemptCount}
			if scenario.terminal {
				task.Status = domain.UploadStatusCancelled
			}
			scheduled := *task
			scheduled.AttemptCount++
			scheduleCalls := 0
			processor := NewUploadCancellationCandidateProcessor(
				func(context.Context, uint64, uint64) (*domain.UploadTask, error) { return task, nil },
				func(context.Context, *domain.UploadTask) (*domain.UploadTask, error) {
					if scenario.stop {
						cancel()
					}
					return nil, scenario.completeError
				},
				func(writeContext context.Context, current *domain.UploadTask, delaySeconds int) (*domain.UploadTask, error) {
					scheduleCalls++
					if writeContext != ctx || current != task || delaySeconds != scenario.wantDelay || delaySeconds == 0 {
						t.Fatal("重试未使用当前任务、原上下文及预期等待时间", current, delaySeconds)
					}
					if scenario.scheduleError != nil {
						return nil, scenario.scheduleError
					}
					return &scheduled, nil
				},
			)

			// 2. 确认只有符合条件的失败会尝试写回，并交付其结果。
			result, err := processor.Process(ctx, candidate)
			wantCalls := 0
			if scenario.wantDelay > 0 {
				wantCalls = 1
			}
			if scheduleCalls != wantCalls || err != scenario.wantError || (err == nil && result != &scheduled) || (err != nil && result != nil) {
				t.Fatal("重试交付结果或写回次数不符", result, err, scheduleCalls)
			}
		})
	}
}
