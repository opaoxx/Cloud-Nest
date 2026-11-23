package service

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// RestoredSyncDeviceBatchResult 汇总一批恢复换代的已确认进度。
// ReachedEnd 表示扫描到末尾。目标开放前仍须完成全量复核。
type RestoredSyncDeviceBatchResult struct {
	Scanned           int
	Reset             int
	NextAfterDeviceID uint64
	ReachedEnd        bool
	FailedUserID      uint64
	FailedDeviceID    uint64
}

// RestoredSyncDeviceBatch 连接恢复设备分页和单设备换代事务。
type RestoredSyncDeviceBatch struct {
	list  func(context.Context, uint64, int) ([]domain.RestoredSyncDeviceCandidate, error)
	reset func(context.Context, domain.RestoredSyncDeviceCandidate) error
}

// NewRestoredSyncDeviceBatch 装配已核对且停服的恢复目标上的仓储方法。
func NewRestoredSyncDeviceBatch(
	list func(context.Context, uint64, int) ([]domain.RestoredSyncDeviceCandidate, error),
	reset func(context.Context, domain.RestoredSyncDeviceCandidate) error,
) *RestoredSyncDeviceBatch {
	if list == nil || reset == nil {
		panic("恢复同步设备批次需要候选分页和单设备换代方法")
	}
	return &RestoredSyncDeviceBatch{list: list, reset: reset}
}

// RunOnce 按设备 ID 执行一批恢复换代，遇到首个失败即返回。
// 首批从 0 开始。失败或取消时，续扫位置只记录确认提交的进度。
func (batch *RestoredSyncDeviceBatch) RunOnce(
	ctx context.Context, afterDeviceID uint64, limit, pageSize int,
) (RestoredSyncDeviceBatchResult, error) {
	// 1. 校验批次上限和页大小，从指定设备位置续扫。
	summary := RestoredSyncDeviceBatchResult{NextAfterDeviceID: afterDeviceID}
	if ctx == nil || limit < 1 || limit > domain.MaxRestoredSyncDeviceCandidates ||
		pageSize < 1 || pageSize > domain.MaxRestoredSyncDeviceCandidates {
		return summary, fmt.Errorf("恢复同步设备批次需要上下文和 1～%d 条的批量及分页上限", domain.MaxRestoredSyncDeviceCandidates)
	}

	// 2. 核对整页候选后，逐条调用单设备换代事务。
	for summary.Scanned < limit {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		pageLimit := min(pageSize, limit-summary.Scanned)
		page, err := batch.list(ctx, summary.NextAfterDeviceID, pageLimit)
		if err != nil {
			return summary, fmt.Errorf("扫描恢复同步设备批次失败: %w", err)
		}
		if !validRestoredSyncDevicePage(page, summary.NextAfterDeviceID, pageLimit) {
			return summary, domain.ErrInvalidSyncDeviceData
		}
		summary.Scanned += len(page)

		// 3. 只记录已确认提交的进度，失败时留在出错设备之前。
		for _, candidate := range page {
			if err := ctx.Err(); err != nil {
				return summary, err
			}
			if err := batch.reset(ctx, candidate); err != nil {
				summary.FailedUserID = candidate.UserID
				summary.FailedDeviceID = candidate.DeviceID
				return summary, fmt.Errorf("重置恢复同步设备失败（账号 %d，设备 %d）: %w", candidate.UserID, candidate.DeviceID, err)
			}
			summary.Reset++
			summary.NextAfterDeviceID = candidate.DeviceID
		}

		if err := ctx.Err(); err != nil {
			return summary, err
		}
		if len(page) < pageLimit {
			summary.ReachedEnd = true
			return summary, nil
		}
	}
	return summary, ctx.Err()
}

func validRestoredSyncDevicePage(page []domain.RestoredSyncDeviceCandidate, afterDeviceID uint64, limit int) bool {
	if len(page) > limit {
		return false
	}
	for _, candidate := range page {
		if candidate.UserID == 0 || candidate.DeviceID <= afterDeviceID || !candidate.State.IsValid() {
			return false
		}
		afterDeviceID = candidate.DeviceID
	}
	return true
}
