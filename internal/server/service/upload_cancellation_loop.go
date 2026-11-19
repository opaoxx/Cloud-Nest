package service

import (
	"context"
	"time"
)

// RunUploadCancellationLoop 立即执行首轮单文件取消处理，随后按间隔串行执行。
// 每轮执行和汇报完成后开始等待。调用方可通过 ctx 停止循环。
func RunUploadCancellationLoop(
	ctx context.Context,
	interval time.Duration,
	runOnce func(context.Context) (UploadCancellationBatchResult, error),
	report func(UploadCancellationBatchResult, error),
) error {
	// 1. 使用取消批次的结果类型，复用即时首轮、轮间等待和退出规则。
	return runUploadBatchLoop(ctx, "定时上传取消收尾", interval, runOnce, report)
}
