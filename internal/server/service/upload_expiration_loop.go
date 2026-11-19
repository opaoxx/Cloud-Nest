package service

import (
	"context"
	"time"
)

// RunUploadExpirationLoop 立即执行首轮单文件过期处理，随后按间隔串行执行。
// 每次执行及汇报完成后开始等待。调用方取消 ctx 可结束循环。
func RunUploadExpirationLoop(
	ctx context.Context,
	interval time.Duration,
	runOnce func(context.Context) (UploadExpirationBatchResult, error),
	report func(UploadExpirationBatchResult, error),
) error {
	// 1. 使用过期批次的结果类型，沿用上传后台任务的共同循环。
	return runUploadBatchLoop(ctx, "定时上传过期处理", interval, runOnce, report)
}
