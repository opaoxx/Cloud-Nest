package service

import (
	"context"
	"time"
)

// RunUploadVerificationLoop 立即执行首轮，此后每轮汇报完成并等待 interval 再继续。
// runOnce 和 report 同步执行。调用方通过 ctx 停止循环，并等待方法返回。
func RunUploadVerificationLoop(
	ctx context.Context,
	interval time.Duration,
	runOnce func(context.Context) (UploadVerificationBatchResult, error),
	report func(UploadVerificationBatchResult, error),
) error {
	// 1. 使用校验批次的结果类型，沿用上传后台任务的共同循环。
	return runUploadBatchLoop(ctx, "定时上传校验", interval, runOnce, report)
}
