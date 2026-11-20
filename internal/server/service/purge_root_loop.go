package service

import (
	"context"
	"time"
)

// RunPurgeRootLoop 立即执行首轮永久清理，每轮汇报后等待 interval。
// runOnce 和 report 同步执行，取消 ctx 后交付本轮结果再退出。
func RunPurgeRootLoop(
	ctx context.Context,
	interval time.Duration,
	runOnce func(context.Context) (PurgeRootBatchResult, error),
	report func(PurgeRootBatchResult, error),
) error {
	// 1. 复用批次循环，按顺序执行清理、汇报和等待。
	return runBatchLoop(ctx, "定时永久清理", interval, runOnce, report)
}
