package main

import (
	"context"
	"errors"
	"io"
	"sync"
)

var (
	errTransferStopping = errors.New("应用正在退出，传输已停止")
	errTransferWake     = errors.New("系统唤醒后已暂停传输，请核对进度后继续")
)

type activeTransfer struct {
	cancel context.CancelCauseFunc
}

// transferLifecycle tracks operations that must release their files before an explicit exit.
type transferLifecycle struct {
	mu      sync.Mutex
	active  map[*activeTransfer]struct{}
	drained chan struct{}
}

func runDesktopTransfer[Result any](parent context.Context, lifecycle *transferLifecycle, run func(context.Context) (Result, error)) (Result, error) {
	if lifecycle == nil {
		return run(parent)
	}
	ctx, finish, err := lifecycle.begin(parent)
	if err != nil {
		var empty Result
		return empty, err
	}
	defer finish()
	result, err := run(ctx)
	var failure *downloadFailure
	if cause := transferInterruption(ctx); cause != nil && err != nil && !errors.As(err, &failure) {
		return result, cause
	}
	return result, err
}

func (lifecycle *transferLifecycle) begin(parent context.Context) (context.Context, func(), error) {
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()

	// 1. 退出开始后拒绝新操作。每次传输用独立的取消信号。
	if lifecycle.drained != nil {
		return nil, nil, errTransferStopping
	}
	if err := parent.Err(); err != nil {
		return nil, nil, err
	}
	if lifecycle.active == nil {
		lifecycle.active = make(map[*activeTransfer]struct{})
	}
	ctx, cancel := context.WithCancelCause(parent)
	operation := &activeTransfer{cancel: cancel}
	lifecycle.active[operation] = struct{}{}
	return ctx, func() {
		cancel(nil)
		lifecycle.mu.Lock()
		defer lifecycle.mu.Unlock()
		delete(lifecycle.active, operation)
		if lifecycle.drained != nil && len(lifecycle.active) == 0 {
			close(lifecycle.drained)
		}
	}, nil
}

func (lifecycle *transferLifecycle) pauseAfterWake() {
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	for operation := range lifecycle.active {
		operation.cancel(errTransferWake)
	}
}

func (lifecycle *transferLifecycle) stop() (<-chan struct{}, bool) {
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()

	// 1. 关闭新任务入口，通知在途任务停止网络和本机复制。
	if lifecycle.drained != nil {
		return lifecycle.drained, false
	}
	lifecycle.drained = make(chan struct{})
	for operation := range lifecycle.active {
		operation.cancel(errTransferStopping)
	}

	// 2. 外层等待传输释放文件和数据库，再结束程序。
	if len(lifecycle.active) == 0 {
		close(lifecycle.drained)
	}
	return lifecycle.drained, true
}

func transferInterruption(ctx context.Context) error {
	switch cause := context.Cause(ctx); cause {
	case errTransferStopping, errTransferWake:
		return cause
	default:
		return nil
	}
}

// transferCheckpointContext lets a confirmed write commit after transfer cancellation.
func transferCheckpointContext(ctx context.Context) context.Context {
	if transferInterruption(ctx) != nil {
		return context.WithoutCancel(ctx)
	}
	return ctx
}

type transferReader struct {
	ctx    context.Context
	source io.Reader
}

func (reader transferReader) Read(body []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.source.Read(body)
}
