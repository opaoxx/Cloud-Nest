package service

import (
	"context"
	"time"
)

// DefaultExpiredTrashRootInterval 是方案约定的回收站到期标记轮间间隔。
const DefaultExpiredTrashRootInterval = time.Hour

// RunExpiredTrashRootLoop 立即执行首轮到期标记，每轮汇报后等待 interval。
// 执行和汇报均在同一循环内串行完成。取消时交付在途轮次结果后退出。
func RunExpiredTrashRootLoop(
	ctx context.Context,
	interval time.Duration,
	runOnce func(context.Context) (ExpiredTrashRootBatchResult, error),
	report func(ExpiredTrashRootBatchResult, error),
) error {
	// 1. 复用批次循环，按顺序执行标记、汇报和等待。
	return runBatchLoop(ctx, "定时标记到期回收站根项", interval, runOnce, report)
}
