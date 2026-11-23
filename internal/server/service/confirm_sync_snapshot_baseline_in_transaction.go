package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// SyncSnapshotBaselineConfirmationTransaction 提供已认证账号事务内的基线确认操作。
type SyncSnapshotBaselineConfirmationTransaction interface {
	FindCurrentSyncDeviceState(context.Context) (domain.SyncDeviceState, error)
	ValidateSyncSnapshotSequence(context.Context, uint64) error
	ActivateSyncDeviceBaseline(context.Context, domain.SyncCursorGeneration, uint64) (bool, error)
}

// ConfirmSyncSnapshotBaselineInTransaction 在账号锁内复核已验签凭据并确认设备位置。
// 调用方须在同一账号事务内执行，提交成功后才可对外返回位置。
func ConfirmSyncSnapshotBaselineInTransaction(
	ctx context.Context, actor domain.Actor, verified *VerifiedSyncSnapshotBaseline, now func() time.Time,
	transaction SyncSnapshotBaselineConfirmationTransaction,
) (uint64, error) {
	// 1. 在锁内复核原期限，只确认当前账号和设备的完整代次。
	if ctx == nil || transaction == nil || now == nil || verified == nil {
		return 0, fmt.Errorf("确认同步快照基线需要事务、已验签凭据、时钟和上下文")
	}
	if actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 {
		return 0, domain.ErrInvalidSessionData
	}
	if verified.UserID != actor.UserID || verified.DeviceID != actor.DeviceID ||
		verified.Generation == (domain.SyncCursorGeneration{}) || verified.ExpiresAt.IsZero() {
		return 0, domain.ErrInvalidSyncSnapshotBaselineToken
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := checkSyncSnapshotBaselineExpiry(now(), verified.ExpiresAt); err != nil {
		return 0, err
	}
	state, err := transaction.FindCurrentSyncDeviceState(ctx)
	if errors.Is(err, domain.ErrSyncDeviceNotFound) {
		return 0, domain.ErrSyncSnapshotExpired
	}
	if err != nil {
		return 0, err
	}
	if state.Generation != verified.Generation {
		return 0, domain.ErrSyncSnapshotExpired
	}

	// 2. 复核凭据能衔接日志。重试还须确保持久位置没有落后或超出账号序号。
	cutoff := verified.HighWaterSequence
	if err := transaction.ValidateSyncSnapshotSequence(ctx, cutoff); err != nil {
		return 0, err
	}
	switch state.Status {
	case domain.SyncDeviceStatusResetRequired:
		if state.AckSequence != 0 {
			return 0, domain.ErrInvalidSyncDeviceData
		}
	case domain.SyncDeviceStatusActive:
		if state.AckSequence < cutoff {
			return 0, domain.ErrInvalidSyncDeviceData
		}
		if state.AckSequence > cutoff {
			if err := transaction.ValidateSyncSnapshotSequence(ctx, state.AckSequence); err != nil {
				return 0, err
			}
		}
	default:
		return 0, domain.ErrInvalidSyncDeviceData
	}

	// 3. 检查读完记录后仍在有效期内。有效重试保持位置，首次确认才调用更新。
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := checkSyncSnapshotBaselineExpiry(now(), verified.ExpiresAt); err != nil {
		return 0, err
	}
	if state.Status == domain.SyncDeviceStatusActive {
		return state.AckSequence, nil
	}
	updated, err := transaction.ActivateSyncDeviceBaseline(ctx, verified.Generation, cutoff)
	if err != nil {
		return 0, err
	}
	if !updated {
		return 0, domain.ErrInvalidSyncDeviceData
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return cutoff, nil
}

func checkSyncSnapshotBaselineExpiry(now, expiresAt time.Time) error {
	if now.IsZero() {
		return fmt.Errorf("确认同步快照基线需要有效的当前时间")
	}
	if !now.Before(expiresAt) {
		return domain.ErrSyncSnapshotExpired
	}
	return nil
}
