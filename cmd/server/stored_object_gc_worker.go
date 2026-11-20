package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

const storedObjectGCInterval = time.Minute

// startServerStoredObjectGCWorker 启动一个对象回收循环，返回取消并等待退出的方法。
func startServerStoredObjectGCWorker(
	ctx context.Context,
	runOnce func(context.Context, int, int) (service.StoredObjectGCRoundResult, error),
	logger *slog.Logger,
) func() {
	// 1. 派生 worker 的取消信号，创建退出通知。
	workerContext, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	// 2. 启动回收循环，汇报两个批次及固定错误码。
	const batchLimit = domain.MaxStoredObjectGCCandidates
	logger.Info("对象回收 worker 已启动", "interval_ms", storedObjectGCInterval.Milliseconds(),
		"recovery_limit", batchLimit, "due_limit", batchLimit)
	go func() {
		defer close(done)
		defer logger.Info("对象回收 worker 已停止")
		var roundStartedAt time.Time
		err := service.RunStoredObjectGCLoop(workerContext, storedObjectGCInterval,
			func(roundContext context.Context) (service.StoredObjectGCRoundResult, error) {
				roundStartedAt = time.Now()
				return runOnce(roundContext, batchLimit, batchLimit)
			},
			func(summary service.StoredObjectGCRoundResult, roundError error) {
				reportStoredObjectGCRound(logger, summary, roundError, workerContext.Err(), time.Since(roundStartedAt))
			})
		if err != nil && workerContext.Err() == nil {
			logger.Error("对象回收 worker 异常退出", "error_code", storedObjectGCWorkerErrorCode(err))
		}
	}()

	// 3. 停止时发出取消信号，等待在途轮次汇报并退出。
	return func() {
		cancel()
		<-done
	}
}

// reportStoredObjectGCRound 记录回收轮次的结果和错误码。
func reportStoredObjectGCRound(logger *slog.Logger, summary service.StoredObjectGCRoundResult, roundError, stopError error, duration time.Duration) {
	// 1. 报告轮次错误，过滤正常关停造成的告警。
	if roundError != nil && (stopError == nil || !errors.Is(roundError, stopError)) {
		logger.Warn("对象回收轮次失败", "error_code", storedObjectGCWorkerErrorCode(roundError))
	}

	// 2. 按阶段报告单项错误。
	for _, phase := range []struct {
		name     string
		failures []service.StoredObjectDeletionCandidateError
	}{
		{"recovery", summary.Recovery.CandidateErrors},
		{"due", summary.Due.CandidateErrors},
	} {
		for _, failure := range phase.failures {
			if stopError != nil && errors.Is(failure.Err, stopError) {
				continue
			}
			logger.Warn("存储对象回收失败", "phase", phase.name, "user_id", failure.OwnerUserID,
				"object_id", failure.ObjectID, "error_code", storedObjectGCWorkerErrorCode(failure.Err))
		}
	}

	// 3. 汇总两个批次的扫描与完成进度。
	if summary.Recovery.Scanned > 0 || summary.Due.Scanned > 0 {
		logger.Info("对象回收轮次结束", "duration_ms", duration.Milliseconds(),
			slog.Group("recovery", "scanned", summary.Recovery.Scanned, "completed", summary.Recovery.Completed,
				"candidate_errors", len(summary.Recovery.CandidateErrors)),
			slog.Group("due", "scanned", summary.Due.Scanned, "completed", summary.Due.Completed,
				"stale_candidates", summary.Due.StaleCandidates, "candidate_errors", len(summary.Due.CandidateErrors)))
	}
}

// storedObjectGCWorkerErrorCode 将错误转换为日志可用的固定码。
func storedObjectGCWorkerErrorCode(err error) string {
	// 1. 根据错误链查找已知原因。
	switch {
	case errors.Is(err, context.Canceled):
		return "CANCELED"
	case errors.Is(err, context.DeadlineExceeded):
		return "DEADLINE_EXCEEDED"
	case errors.Is(err, domain.ErrInvalidAccountData):
		return "INVALID_ACCOUNT_DATA"
	case errors.Is(err, domain.ErrAccountNotFound):
		return "ACCOUNT_NOT_FOUND"
	case errors.Is(err, domain.ErrInvalidStoredObjectData):
		return "INVALID_STORED_OBJECT_DATA"
	case errors.Is(err, domain.ErrStoredObjectInUse):
		return "STORED_OBJECT_IN_USE"
	case errors.Is(err, domain.ErrStoredObjectDeleteUnavailable):
		return "STORED_OBJECT_DELETE_UNAVAILABLE"
	default:
		return "INTERNAL_ERROR"
	}
}
