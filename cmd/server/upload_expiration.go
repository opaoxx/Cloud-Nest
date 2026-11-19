package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

const (
	uploadExpirationInterval   = 30 * time.Second
	uploadExpirationBatchLimit = 100
)

// newServerUploadExpirationBatch 装配单文件过期处理的正式仓储依赖。
func newServerUploadExpirationBatch(store *repository.Store) *service.UploadExpirationBatch {
	// 1. 装配单文件到期扫描、阶段推进和收尾。
	processor := service.NewUploadExpirationCandidateProcessor(store.BeginUploadExpiration, store.CompleteUploadExpiration)
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
				if roundError != nil && workerContext.Err() == nil {
					logger.Warn("上传过期轮次失败")
				}
				if summary.Scanned > 0 {
					logger.Info("上传过期轮次结束", "scanned", summary.Scanned, "expired", summary.Expired,
						"candidate_errors", len(summary.CandidateErrors))
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
