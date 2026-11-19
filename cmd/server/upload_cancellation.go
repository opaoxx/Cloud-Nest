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

const uploadCancellationInterval = 30 * time.Second

// newServerUploadCancellationBatch 用服务持有的 MySQL 仓储装配单文件取消收尾。
func newServerUploadCancellationBatch(store *repository.Store) *service.UploadCancellationBatch {
	// 1. 绑定 MySQL 候选扫描、任务读取和取消收尾事务。
	processor := service.NewUploadCancellationCandidateProcessor(store.FindUserUploadTask, store.CompleteUploadCancellation)
	return service.NewUploadCancellationBatch(store.ListSingleUploadCancellationCandidates, processor.Process)
}

// startServerUploadCancellationWorker 启动一个取消循环，返回取消并等待的方法。
func startServerUploadCancellationWorker(ctx context.Context, runOnce func(context.Context, int) (service.UploadCancellationBatchResult, error), logger *slog.Logger) func() {
	// 1. 从服务上下文派生停止信号，用 done 表示 worker 已退出。
	workerContext, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	// 2. 启动后台循环，立即执行首轮，每轮最多处理 100 条，汇报后等待 30 秒。
	logger.Info("上传取消 worker 已启动", "interval_ms", uploadCancellationInterval.Milliseconds(), "batch_limit", domain.MaxUploadCancellationCandidates)
	go func() {
		defer close(done)
		defer logger.Info("上传取消 worker 已停止")
		err := service.RunUploadCancellationLoop(workerContext, uploadCancellationInterval,
			func(roundContext context.Context) (service.UploadCancellationBatchResult, error) {
				return runOnce(roundContext, domain.MaxUploadCancellationCandidates)
			},
			func(summary service.UploadCancellationBatchResult, roundError error) {
				stopError := workerContext.Err()
				if roundError != nil && stopError == nil {
					logger.Warn("上传取消轮次失败")
				}
				// 1. 遍历失败任务，跳过随服务关停产生的上下文错误。
				for _, failure := range summary.CandidateErrors {
					if stopError != nil && errors.Is(failure.Err, stopError) {
						continue
					}
					// 2. 把收尾错误映射为固定码，未知原因归为 INTERNAL_ERROR。
					code := uploadCleanupErrorCode(failure.Err)
					// 3. 记录任务 ID 和错误码，再汇报本轮的完整失败数。
					logger.Warn("上传取消任务处理失败", "task_id", failure.TaskID, "error_code", code)
				}
				if summary.Scanned > 0 {
					logger.Info("上传取消轮次结束", "scanned", summary.Scanned,
						"cancelled", summary.Cancelled, "candidate_errors", len(summary.CandidateErrors))
				}
			})
		if err != nil && workerContext.Err() == nil {
			logger.Error("上传取消 worker 异常退出")
		}
	}()

	// 3. 服务退出时发送停止信号，等循环返回后再关闭依赖连接。
	return func() {
		cancel()
		<-done
	}
}
