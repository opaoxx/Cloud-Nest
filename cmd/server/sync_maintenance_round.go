package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/service"
)

// syncMaintenanceRoundResult 保留两个阶段各自的结果与续扫位置。
type syncMaintenanceRoundResult struct {
	Offline      service.OfflineSyncDeviceBatchResult
	Prune        service.SyncChangePruneBatchResult
	PruneStarted bool
}

// runServerSyncMaintenanceRound 在同一轮中先完成离线设备重建，再执行日志裁剪。
func runServerSyncMaintenanceRound(
	ctx context.Context, afterDeviceID, afterUserID uint64,
	runOffline func(context.Context, uint64) (service.OfflineSyncDeviceBatchResult, error),
	runPrune func(context.Context, uint64) (service.SyncChangePruneBatchResult, error),
) (syncMaintenanceRoundResult, error) {
	// 1. 保存两项任务的续扫位置，检查上下文。
	result := syncMaintenanceRoundResult{
		Offline: service.OfflineSyncDeviceBatchResult{NextAfterDeviceID: afterDeviceID},
		Prune:   service.SyncChangePruneBatchResult{NextAfterUserID: afterUserID},
	}
	if ctx == nil || runOffline == nil || runPrune == nil {
		return result, errors.New("同步维护轮次需要上下文、离线重建和日志裁剪入口")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}

	// 2. 同步执行离线重建。整轮未完成时保留其部分进度，裁剪留到后续轮次。
	offline, err := runOffline(ctx, afterDeviceID)
	result.Offline = offline
	if err != nil {
		return result, fmt.Errorf("离线设备重建轮次未完成: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}

	// 3. 用已提交的设备状态裁剪。单设备失败留在离线结果中，由裁剪事务按实际状态复核。
	result.PruneStarted = true
	result.Prune, err = runPrune(ctx, afterUserID)
	return result, err
}
