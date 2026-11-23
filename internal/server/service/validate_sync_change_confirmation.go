package service

import "github.com/opaoxx/cloud-nest/internal/server/domain"

// SyncChangeConfirmationDecision 保存锁内复核得出的确认目标及是否记录活动。
type SyncChangeConfirmationDecision struct {
	AckSequence    uint64
	RecordActivity bool
}

// ValidateSyncChangeConfirmation 复核已验签的增量确认位置。
// 设备状态和日志区间须来自同一已认证账号事务，返回决定只供当前事务使用。
func ValidateSyncChangeConfirmation(
	actor domain.Actor, position domain.SyncChangeCursorPosition, state domain.SyncDeviceState, bounds domain.SyncChangeSequenceBounds,
) (SyncChangeConfirmationDecision, error) {
	// 1. 核对认证身份与游标绑定。
	if actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 {
		return SyncChangeConfirmationDecision{}, domain.ErrInvalidSessionData
	}
	if position.UserID != actor.UserID || position.DeviceID != actor.DeviceID || position.Generation == (domain.SyncCursorGeneration{}) {
		return SyncChangeConfirmationDecision{}, domain.ErrInvalidSyncChangeCursor
	}

	// 2. 检查设备状态、完整代次和日志区间。
	if err := validateSyncChangePosition(position, state, bounds, domain.ErrInvalidSyncChangeCursor); err != nil {
		return SyncChangeConfirmationDecision{}, err
	}

	// 3. 较旧位置保留当前确认值。更高或等值位置均记录活动。
	if position.Sequence < state.AckSequence {
		return SyncChangeConfirmationDecision{AckSequence: state.AckSequence}, nil
	}
	return SyncChangeConfirmationDecision{AckSequence: position.Sequence, RecordActivity: true}, nil
}
