package service

import (
	"context"
	"errors"
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
