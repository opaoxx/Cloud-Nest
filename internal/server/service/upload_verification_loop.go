package service

import (
	"context"
	"errors"
	"time"
)

// RunUploadVerificationLoop 立即执行首轮，此后每轮汇报完成并等待 interval 再继续。
// runOnce 和 report 同步执行。调用方通过 ctx 停止循环，并等待方法返回。
func RunUploadVerificationLoop(
	ctx context.Context,
	interval time.Duration,
	runOnce func(context.Context) (UploadVerificationBatchResult, error),
	report func(UploadVerificationBatchResult, error),
) error {
	// 1. 核对上下文、间隔和两个回调。
	if ctx == nil || interval <= 0 || runOnce == nil || report == nil {
		return errors.New("定时上传校验需要上下文、正间隔、单轮执行和结果回调")
	}

	for {
		// 2. 串行执行一轮校验并交付本轮汇总和错误。
		if err := ctx.Err(); err != nil {
			return err
		}
		summary, roundError := runOnce(ctx)
		report(summary, roundError)

		// 3. 等待下一轮间隔，或响应取消退出。
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
