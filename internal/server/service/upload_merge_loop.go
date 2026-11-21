package service

import (
	"context"
	"time"
)

// RunUploadMergeLoop 立即执行首轮，此后每轮汇报完成并等待 interval 再继续。
// 执行和汇报都是同步的。取消时先汇报在途轮次的结果，再返回取消原因。
func RunUploadMergeLoop(
	ctx context.Context,
	interval time.Duration,
	runOnce func(context.Context) (UploadMergeBatchResult, error),
	report func(UploadMergeBatchResult, error),
) error {
	// 1. 复用已有批次循环，依次执行单轮合并、汇报和等待。
	return runBatchLoop(ctx, "定时分片合并", interval, runOnce, report)
}
