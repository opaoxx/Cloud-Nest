package service

import (
	"context"
	"errors"
	"time"
)

// RunOfflineSyncDeviceLoop 立即执行首轮离线重建，每轮汇报后等待 interval。
// 单轮回调沿用 RunOnce 的续扫约定，出错时仍返回可继续的位置。
func RunOfflineSyncDeviceLoop(
	ctx context.Context,
	interval time.Duration,
	runOnce func(context.Context, uint64) (OfflineSyncDeviceBatchResult, error),
	report func(OfflineSyncDeviceBatchResult, error),
) error {
	// 1. 校验单轮回调，初始化续扫位置。
	if runOnce == nil {
		return errors.New("定时重建离线设备需要单轮执行回调")
	}
	var afterDeviceID uint64

	// 2. 执行一轮并保存返回的续扫位置。
	executeRound := func(roundContext context.Context) (OfflineSyncDeviceBatchResult, error) {
		summary, err := runOnce(roundContext, afterDeviceID)
		afterDeviceID = summary.NextAfterDeviceID
		return summary, err
	}

	// 3. 复用共用循环，串行汇报、等待并响应取消。
	return runBatchLoop(ctx, "定时重建离线设备", interval, executeRound, report)
}
