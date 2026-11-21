package service

import (
	"context"
	"time"
)

const uploadLeaseRenewalInterval = 20 * time.Second

// startUploadLeaseRenewal 在领取事务提交后，为本次存储操作定时续租。
// operationContext 和 cancelOperation 由调用方通过 context.WithCancelCause 成对创建。
// renew 同步执行一次有期限的短事务，响应请求取消。
// 调用方在写回前停止循环，并在所有退出路径释放操作上下文。
func startUploadLeaseRenewal(operationContext context.Context, cancelOperation context.CancelCauseFunc, renew func(context.Context) error) func() error {
	// 1. 为本次操作创建独立的定时循环。
	loopContext, stopRenewing := context.WithCancel(operationContext)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer stopRenewing()
		ticker := time.NewTicker(uploadLeaseRenewalInterval)
		defer ticker.Stop()

		// 2. 每 20 秒串行续租，失败时取消当前操作。
		for {
			select {
			case <-loopContext.Done():
				return
			case <-ticker.C:
				if loopContext.Err() != nil {
					return
				}
				// 正常停止时等待正在执行的事务，请求取消仍能传给事务。
				if err := renew(operationContext); err != nil {
					cancelOperation(err)
					return
				}
			}
		}
	}()

	// 3. 停止时等待循环退出，并返回操作取消原因。
	return func() error {
		stopRenewing()
		<-done
		return context.Cause(operationContext)
	}
}
