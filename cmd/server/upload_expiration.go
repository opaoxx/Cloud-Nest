package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

const (
	uploadExpirationInterval   = 30 * time.Second
	uploadExpirationBatchLimit = 100
)

// newServerUploadExpirationBatch 装配单文件过期处理的正式仓储依赖。
func newServerUploadExpirationBatch(store *repository.Store) *service.UploadExpirationBatch {
	// 1. 装配单文件到期扫描、阶段推进、收尾和重试写回。
	processor := service.NewUploadExpirationCandidateProcessor(
		store.BeginUploadExpiration, store.CompleteUploadExpiration, store.ScheduleUploadExpirationRetry,
	)
	return service.NewUploadExpirationBatch(store.ListSingleUploadExpirationCandidates, processor.Process)
}

// startServerUploadExpirationWorker 启动过期处理并返回可以等待退出的停止方法。
func startServerUploadExpirationWorker(ctx context.Context, runOnce func(context.Context, int) (service.UploadExpirationBatchResult, error), logger *slog.Logger) func() {
	// 1. 创建随服务退出的上下文和完成通知。
	workerContext, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	// 2. 启动定时循环，汇报扫描数、终结数和错误数。
	logger.Info("上传过期 worker 已启动", "interval_ms", uploadExpirationInterval.Milliseconds(), "batch_limit", uploadExpirationBatchLimit)
	go func() {
		defer close(done)
		defer logger.Info("上传过期 worker 已停止")
		err := service.RunUploadExpirationLoop(workerContext, uploadExpirationInterval,
			func(roundContext context.Context) (service.UploadExpirationBatchResult, error) {
				return runOnce(roundContext, uploadExpirationBatchLimit)
			},
			func(summary service.UploadExpirationBatchResult, roundError error) {
				stopError := workerContext.Err()
				if roundError != nil && stopError == nil {
					logger.Warn("上传过期轮次失败")
				}
				// 1. 遍历本轮失败任务，跳过由服务关停引起的取消。
				for _, failure := range summary.CandidateErrors {
					if stopError != nil && errors.Is(failure.Err, stopError) {
						continue
					}
					// 2. 把错误转换为固定码，未知原因归为 INTERNAL_ERROR。
					code := uploadExpirationErrorCode(failure.Err)
					// 3. 写入任务 ID 和错误码，再汇报整轮计数。
					logger.Warn("上传过期任务处理失败", "task_id", failure.TaskID, "error_code", code)
				}
				if summary.Scanned > 0 {
					logger.Info("上传过期轮次结束", "scanned", summary.Scanned, "expired", summary.Expired,
						"retries_scheduled", summary.RetriesScheduled, "candidate_errors", len(summary.CandidateErrors))
				}
			})
		if err != nil && workerContext.Err() == nil {
			logger.Error("上传过期 worker 异常退出", "error", err)
		}
	}()

	// 3. 退出时取消并等待 worker，再关闭数据库连接。
	return func() {
		cancel()
		<-done
	}
}

// uploadExpirationErrorCode 为后台过期日志返回固定错误码。
func uploadExpirationErrorCode(err error) string {
	// 1. 识别仍被引用而无法进入回收流程的暂存对象。
	if errors.Is(err, domain.ErrStoredObjectInUse) {
		return "STORED_OBJECT_IN_USE"
	}

	// 2. 其余原因交给上传 worker 共用的错误码映射。
	return uploadWorkerErrorCode(err)
}
