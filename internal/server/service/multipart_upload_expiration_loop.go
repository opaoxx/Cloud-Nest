package service

import (
	"context"
	"time"
)

// RunMultipartUploadExpirationLoop 立即执行首轮，随后串行汇报并等待轮间隔。
// 单轮错误随结果汇报，调用取消时交付在途结果后返回停止原因。
func RunMultipartUploadExpirationLoop(
	ctx context.Context,
	interval time.Duration,
	runOnce func(context.Context) (MultipartUploadExpirationBatchResult, error),
	report func(MultipartUploadExpirationBatchResult, error),
) error {
	// 1. 复用批次循环的立即首轮、同步汇报、轮间等待和取消规则。
	return runBatchLoop(ctx, "定时分片到期收尾", interval, runOnce, report)
}
