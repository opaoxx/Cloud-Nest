package service

import (
	"context"
	"time"
)

// RunExpiredFileVersionLoop 立即执行首轮历史结算，每轮汇报后等待 interval。
// 单轮执行和汇报均串行完成，取消时交付在途轮次结果后退出。
func RunExpiredFileVersionLoop(
	ctx context.Context,
	interval time.Duration,
	runOnce func(context.Context) (ExpiredFileVersionBatchResult, error),
	report func(ExpiredFileVersionBatchResult, error),
) error {
	// 1. 复用批次循环，按顺序执行单轮结算、汇报和等待。
	return runBatchLoop(ctx, "定时结算到期历史版本", interval, runOnce, report)
}
