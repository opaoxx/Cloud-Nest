package service

import (
	"context"
	"time"
)

const uploadContentRenewalInterval = 20 * time.Second

// startUploadContentLeaseRenewal 在准备事务提交后，为本次接收定时续租。
// receiveContext 和 cancelReceive 由调用方通过 context.WithCancelCause 成对创建。
// renew 同步执行一次有期限的短事务，响应请求取消。
// 调用方在所有退出路径释放接收上下文，并在推进校验前调用返回的停止方法。
func startUploadContentLeaseRenewal(receiveContext context.Context, cancelReceive context.CancelCauseFunc, renew func(context.Context) error) func() error {
	// 1. 为本次接收创建独立的定时循环。
	loopContext, stopRenewing := context.WithCancel(receiveContext)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer stopRenewing()
		ticker := time.NewTicker(uploadContentRenewalInterval)
		defer ticker.Stop()

		// 2. 每 20 秒串行续租，失败时取消接收。
		for {
			select {
			case <-loopContext.Done():
				return
			case <-ticker.C:
				if loopContext.Err() != nil {
					return
				}
				// 正常停止时等待正在执行的事务，请求取消仍能传给事务。
				if err := renew(receiveContext); err != nil {
					cancelReceive(err)
					return
				}
			}
		}
	}()

	// 3. 停止时等待循环退出，并返回接收取消原因。
	return func() error {
		stopRenewing()
		<-done
		return context.Cause(receiveContext)
	}
}
