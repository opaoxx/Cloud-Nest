package service

import (
	"context"
	"time"
)

// RunMultipartUploadInitializationLoop 立即执行首轮初始化，每轮汇报后等待 interval。
// 执行和汇报同步完成，取消时交付在途轮次的结果后返回。
func RunMultipartUploadInitializationLoop(
	ctx context.Context,
	interval time.Duration,
	runOnce func(context.Context) (MultipartUploadInitializationBatchResult, error),
	report func(MultipartUploadInitializationBatchResult, error),
) error {
	// 1. 复用批次循环，依次完成单轮初始化、结果汇报和间隔等待。
	return runBatchLoop(ctx, "定时分片上传初始化", interval, runOnce, report)
}
