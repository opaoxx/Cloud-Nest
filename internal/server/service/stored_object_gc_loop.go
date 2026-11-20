package service

import (
	"context"
	"time"
)

// RunStoredObjectGCLoop 立即执行首轮对象回收，随后按间隔串行执行。
// 每轮执行和汇报完成后开始等待，取消上下文可结束循环。
func RunStoredObjectGCLoop(
	ctx context.Context,
	interval time.Duration,
	runOnce func(context.Context) (StoredObjectGCRoundResult, error),
	report func(StoredObjectGCRoundResult, error),
) error {
	// 1. 复用后台批次循环的即时首轮、轮间等待和取消规则。
	return runBatchLoop(ctx, "定时存储对象回收", interval, runOnce, report)
}
