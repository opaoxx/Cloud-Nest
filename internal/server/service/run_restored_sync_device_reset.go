package service

import (
	"context"
	"fmt"
	"slices"
	"sort"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// RestoredSyncDeviceResetResult 汇总本次确认提交和最终读回通过的条数。
type RestoredSyncDeviceResetResult struct {
	Reset    int
	Verified int
}

// RunRestoredSyncDeviceReset 在已核对且停服的目标上执行同步换代。
// 首次运行传入 nil 清单，保存方法必须在持久保存原清单后返回成功，
// 遇到已有清单时返回错误。重跑传入原清单及 nil 保存方法，空清单用非 nil 空切片表示。
// 每次均从头读取当前状态。只有成功返回才表示设备全量复核通过。
func RunRestoredSyncDeviceReset(
	ctx context.Context, imported []domain.RestoredSyncDeviceCandidate, batchSize, pageSize int,
	list func(context.Context, uint64, int) ([]domain.RestoredSyncDeviceCandidate, error),
	reset func(context.Context, domain.RestoredSyncDeviceCandidate) error,
	saveImported func(context.Context, []domain.RestoredSyncDeviceCandidate) error,
) (RestoredSyncDeviceResetResult, error) {
	// 1. 首次运行先保存原始清单，重跑时传入原清单。
	var result RestoredSyncDeviceResetResult
	if ctx == nil || list == nil || reset == nil || (imported == nil) == (saveImported == nil) ||
		batchSize < 1 || batchSize > domain.MaxRestoredSyncDeviceCandidates ||
		pageSize < 1 || pageSize > domain.MaxRestoredSyncDeviceCandidates {
		return result, fmt.Errorf("恢复同步设备需要有效依赖和批次参数，并指定首次保存或原始导入清单")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if imported == nil {
		var err error
		imported, err = readRestoredSyncDeviceRoster(ctx, pageSize, list)
		if err != nil {
			return result, err
		}
		if err := saveImported(ctx, slices.Clone(imported)); err != nil {
			return result, fmt.Errorf("保存恢复同步设备原始清单失败: %w", err)
		}
	} else if !validRestoredSyncDevicePage(imported, 0, len(imported)) {
		return result, fmt.Errorf("恢复同步设备导入清单无效: %w", domain.ErrInvalidSyncDeviceData)
	}

	// 2. 逐页核对设备归属，再分批提交换代。
	batch := NewRestoredSyncDeviceBatch(func(queryContext context.Context, afterDeviceID uint64, limit int) ([]domain.RestoredSyncDeviceCandidate, error) {
		page, err := list(queryContext, afterDeviceID, limit)
		if err != nil {
			return nil, err
		}
		firstExpected := sort.Search(len(imported), func(index int) bool {
			return imported[index].DeviceID > afterDeviceID
		})
		expectedCount := min(limit, len(imported)-firstExpected)
		if len(page) != expectedCount {
			return nil, fmt.Errorf("%w: 设备 %d 之后应读取 %d 条，实际读取 %d 条",
				ErrRestoredSyncDeviceResetIncomplete, afterDeviceID, expectedCount, len(page))
		}
		for index, current := range page {
			expected := imported[firstExpected+index]
			if current.UserID != expected.UserID || current.DeviceID != expected.DeviceID {
				return nil, fmt.Errorf("%w: 期望账号 %d 设备 %d，实际账号 %d 设备 %d",
					ErrRestoredSyncDeviceResetIncomplete, expected.UserID, expected.DeviceID, current.UserID, current.DeviceID)
			}
		}
		return page, nil
	}, reset)
	var afterDeviceID uint64
	for {
		batchResult, err := batch.RunOnce(ctx, afterDeviceID, batchSize, pageSize)
		result.Reset += batchResult.Reset
		if err != nil {
			return result, err
		}
		if batchResult.ReachedEnd {
			break
		}
		afterDeviceID = batchResult.NextAfterDeviceID
	}

	// 3. 从头独立读回，确认全量换代结果。
	verified, err := VerifyRestoredSyncDeviceReset(ctx, imported, pageSize, list)
	result.Verified = verified
	return result, err
}

func readRestoredSyncDeviceRoster(
	ctx context.Context, pageSize int,
	list func(context.Context, uint64, int) ([]domain.RestoredSyncDeviceCandidate, error),
) ([]domain.RestoredSyncDeviceCandidate, error) {
	// 1. 从第一台设备开始分页，保留明确的空清单。
	imported := make([]domain.RestoredSyncDeviceCandidate, 0)
	var afterDeviceID uint64
	for {
		page, err := list(ctx, afterDeviceID, pageSize)
		if err != nil {
			return nil, fmt.Errorf("读取恢复同步设备原始清单失败: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !validRestoredSyncDevicePage(page, afterDeviceID, pageSize) {
			return nil, domain.ErrInvalidSyncDeviceData
		}

		// 2. 收下已核对的页，读到短页或空页时结束。
		imported = append(imported, page...)
		if len(page) < pageSize {
			return imported, nil
		}
		afterDeviceID = page[len(page)-1].DeviceID
	}
}
