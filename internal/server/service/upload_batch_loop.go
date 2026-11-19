package service

import (
	"context"
	"errors"
	"time"
)

// runUploadBatchLoop 负责在各轮完成和汇报后等待指定间隔。
func runUploadBatchLoop[Result any](
	ctx context.Context,
	operation string,
	interval time.Duration,
	runOnce func(context.Context) (Result, error),
	report func(Result, error),
) error {
	// 1. 检查上下文、间隔和回调。
	if ctx == nil || interval <= 0 || runOnce == nil || report == nil {
		return errors.New(operation + "需要上下文、正间隔、单轮执行和结果回调")
	}

	for {
		// 2. 串行执行一轮，将结果和错误交给汇报回调。
		if err := ctx.Err(); err != nil {
			return err
		}
		summary, roundError := runOnce(ctx)
		report(summary, roundError)

		// 3. 等待下一轮，或在取消时退出。
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
