package service

import (
	"context"
	"errors"
	"time"
)

// RunMultipartUploadSessionCleanupLoop 串行驱动会话收尾，并在一次运行内保留续扫位置。
// runOnce 与 RunOnce 使用相同的约定，Next 始终给出可继续的位置。
func RunMultipartUploadSessionCleanupLoop(
	ctx context.Context,
	interval time.Duration,
	runOnce func(context.Context, MultipartUploadSessionCleanupPosition) (MultipartUploadSessionCleanupRoundResult, error),
	report func(MultipartUploadSessionCleanupRoundResult, error),
) error {
	// 1. 校验单轮回调，初始化继续位置。
	if runOnce == nil {
		return errors.New("定时分片会话收尾需要单轮执行回调")
	}
	var position MultipartUploadSessionCleanupPosition

	// 2. 执行一轮。扫完后归零，其余情况沿用本轮返回的位置。
	executeRound := func(roundContext context.Context) (MultipartUploadSessionCleanupRoundResult, error) {
		summary, err := runOnce(roundContext, position)
		position = summary.Next
		if err == nil && summary.Done {
			position = MultipartUploadSessionCleanupPosition{}
		}
		return summary, err
	}

	// 3. 用共用循环串行执行与汇报，并等待下一轮或响应取消。
	return runBatchLoop(ctx, "定时分片会话收尾", interval, executeRound, report)
}
