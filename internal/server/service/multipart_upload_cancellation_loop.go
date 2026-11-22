package service

import (
	"context"
	"time"
)

// RunMultipartUploadCancellationLoop 立即执行首轮，随后串行汇报并等待轮间隔。
// 单轮错误随该轮结果汇报。取消时先交付在途轮次的结果，再返回停止原因。
func RunMultipartUploadCancellationLoop(
	ctx context.Context,
	interval time.Duration,
	runOnce func(context.Context) (MultipartUploadCancellationBatchResult, error),
	report func(MultipartUploadCancellationBatchResult, error),
) error {
	// 1. 复用批次循环的立即首轮、同步汇报、轮间等待和取消规则。
	return runBatchLoop(ctx, "定时分片取消收尾", interval, runOnce, report)
}
