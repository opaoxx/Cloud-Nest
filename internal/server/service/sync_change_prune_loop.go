package service

import (
	"context"
	"errors"
	"time"
)

// RunSyncChangePruneLoop 立即执行首轮，然后串行汇报并等待 interval。
// 单轮函数在失败时也返回可继续的账号位置，循环将它交给下一轮。
func RunSyncChangePruneLoop(
	ctx context.Context,
	interval time.Duration,
	runOnce func(context.Context, uint64) (SyncChangePruneBatchResult, error),
	report func(SyncChangePruneBatchResult, error),
) error {
	// 1. 校验单轮函数，从账号游标 0 开始。
	if runOnce == nil {
		return errors.New("定时裁剪同步日志需要单轮执行回调")
	}
	var afterUserID uint64

	// 2. 执行一轮，保存下一轮的续扫位置。
	executeRound := func(roundContext context.Context) (SyncChangePruneBatchResult, error) {
		summary, err := runOnce(roundContext, afterUserID)
		afterUserID = summary.NextAfterUserID
		return summary, err
	}

	// 3. 交给共用循环串行汇报、等待和响应取消。
	return runBatchLoop(ctx, "定时裁剪同步日志", interval, executeRound, report)
}
