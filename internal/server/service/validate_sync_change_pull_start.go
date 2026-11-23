package service

import (
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ValidateSyncChangePullStart 复核已验签的起点能否读取本页。
// 设备状态和日志区间须来自同一账号读视图，now 在取得这些数据后采样。
func ValidateSyncChangePullStart(actor domain.Actor, start *SyncChangePullStart, state domain.SyncDeviceState, bounds domain.SyncChangeSequenceBounds, now time.Time) error {
	// 1. 核对起点归属，复查首次凭据的原期限。
	if start == nil {
		return fmt.Errorf("复核增量拉取起点需要已验签的位置")
	}
	if actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 {
		return domain.ErrInvalidSessionData
	}
	fromBaseline := !start.BaselineExpiresAt.IsZero()
	credentialError := domain.ErrInvalidSyncChangeCursor
	if fromBaseline {
		credentialError = domain.ErrInvalidSyncSnapshotBaselineToken
	}
	position := start.Position
	if position.UserID != actor.UserID || position.DeviceID != actor.DeviceID || position.Generation == (domain.SyncCursorGeneration{}) {
		return credentialError
	}
	if fromBaseline {
		if err := checkSyncSnapshotBaselineExpiry(now, start.BaselineExpiresAt); err != nil {
			return err
		}
	}

	// 2. 在同一视图中检查设备状态、代次和日志区间。
	if err := validateSyncChangePosition(position, state, bounds, credentialError); err != nil {
		return err
	}

	// 3. 首次拉取还须核对设备已确认的基线。
	if fromBaseline && state.AckSequence < position.Sequence {
		return domain.ErrInvalidSyncDeviceData
	}
	return nil
}

// validateSyncChangePosition 复用拉取和增量确认对同一账号状态与区间的检查。
func validateSyncChangePosition(position domain.SyncChangeCursorPosition, state domain.SyncDeviceState, bounds domain.SyncChangeSequenceBounds, credentialError error) error {
	// 1. 检查设备状态和完整代次。
	if state.Generation == (domain.SyncCursorGeneration{}) {
		return domain.ErrInvalidSyncDeviceData
	}
	switch state.Status {
	case domain.SyncDeviceStatusActive:
	case domain.SyncDeviceStatusResetRequired:
		if state.AckSequence != 0 {
			return domain.ErrInvalidSyncDeviceData
		}
		return domain.ErrSyncSnapshotExpired
	default:
		return domain.ErrInvalidSyncDeviceData
	}
	if position.Generation != state.Generation {
		return domain.ErrSyncSnapshotExpired
	}

	// 2. 核对日志区间、已有确认位置和签名位置。
	if bounds.MinimumPosition > bounds.HighWaterSequence {
		return domain.ErrInvalidSyncSnapshotSequence
	}
	if state.AckSequence > bounds.HighWaterSequence {
		return domain.ErrInvalidSyncDeviceData
	}
	if position.Sequence > bounds.HighWaterSequence {
		return credentialError
	}
	if position.Sequence < bounds.MinimumPosition {
		return domain.ErrSyncSnapshotExpired
	}
	return nil
}
