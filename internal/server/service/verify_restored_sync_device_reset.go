package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ErrRestoredSyncDeviceResetIncomplete 表示恢复目标的设备范围或换代结果尚未通过复核。
var ErrRestoredSyncDeviceResetIncomplete = errors.New("恢复同步设备换代复核未通过")

// VerifyRestoredSyncDeviceReset 复核停服恢复目标的全部同步行。
// imported 按设备 ID 升序，来自导入后、换代前的同一目标。明确的空清单使用非 nil 空切片。
// 返回值是已比对通过的设备数。只有没有错误时才表示全量复核通过。
func VerifyRestoredSyncDeviceReset(
	ctx context.Context, imported []domain.RestoredSyncDeviceCandidate, pageSize int,
	list func(context.Context, uint64, int) ([]domain.RestoredSyncDeviceCandidate, error),
) (int, error) {
	// 1. 校验导入清单和分页参数。
	if ctx == nil || imported == nil || list == nil || pageSize < 1 || pageSize > domain.MaxRestoredSyncDeviceCandidates {
		return 0, fmt.Errorf("复核恢复设备换代需要上下文、导入清单、目标读取和有效页大小")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if !validRestoredSyncDevicePage(imported, 0, len(imported)) {
		return 0, fmt.Errorf("恢复同步设备导入清单无效: %w", domain.ErrInvalidSyncDeviceData)
	}

	// 2. 按设备 ID 逐页读取目标当前状态，与原记录比对。
	verified := 0
	var afterDeviceID uint64
	for {
		page, err := list(ctx, afterDeviceID, pageSize)
		if err != nil {
			return verified, fmt.Errorf("读取恢复同步设备复核页失败: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return verified, err
		}
		if !validRestoredSyncDevicePage(page, afterDeviceID, pageSize) {
			return verified, domain.ErrInvalidSyncDeviceData
		}
		for _, current := range page {
			if verified == len(imported) {
				return verified, fmt.Errorf("%w: 目标存在额外设备 %d", ErrRestoredSyncDeviceResetIncomplete, current.DeviceID)
			}
			expected := imported[verified]
			if current.DeviceID != expected.DeviceID || current.UserID != expected.UserID {
				return verified, fmt.Errorf("%w: 期望账号 %d 设备 %d，实际账号 %d 设备 %d",
					ErrRestoredSyncDeviceResetIncomplete, expected.UserID, expected.DeviceID, current.UserID, current.DeviceID)
			}
			if current.State.Status != domain.SyncDeviceStatusResetRequired || current.State.Generation == expected.State.Generation {
				return verified, fmt.Errorf("%w: 账号 %d 设备 %d 尚未完成换代",
					ErrRestoredSyncDeviceResetIncomplete, expected.UserID, expected.DeviceID)
			}
			verified++
			afterDeviceID = current.DeviceID
		}

		// 3. 确认两边都已读完，再返回全部设备通过。
		if err := ctx.Err(); err != nil {
			return verified, err
		}
		if len(page) < pageSize {
			if verified != len(imported) {
				return verified, fmt.Errorf("%w: 目标缺少从设备 %d 开始的同步记录",
					ErrRestoredSyncDeviceResetIncomplete, imported[verified].DeviceID)
			}
			return verified, nil
		}
	}
}
