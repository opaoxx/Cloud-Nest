package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// SyncChangeConfirmationTransaction 提供已认证账号事务内的增量确认操作。
type SyncChangeConfirmationTransaction interface {
	FindCurrentSyncDeviceState(context.Context) (domain.SyncDeviceState, error)
	ReadCurrentSyncChangeSequenceBounds(context.Context) (domain.SyncChangeSequenceBounds, error)
	RecordSyncChangeConfirmation(ctx context.Context, generation domain.SyncCursorGeneration, expectedSequence, confirmedSequence uint64) (bool, error)
}

// ConfirmSyncChangesInTransaction 在账号锁内复核已验签的游标，并记录本次确认。
// 调用方须使用同一 WithUserTransaction 回调的仓储和上下文，提交成功后才可交付位置。
func ConfirmSyncChangesInTransaction(
	ctx context.Context, actor domain.Actor, verified *domain.SyncChangeCursorPosition, transaction SyncChangeConfirmationTransaction,
) (uint64, error) {
	// 1. 核对身份并锁读当前设备状态。
	if ctx == nil || transaction == nil || verified == nil {
		return 0, fmt.Errorf("确认增量变更需要账号事务、已验签游标和上下文")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	position := *verified
	if err := validateSyncChangeConfirmationIdentity(actor, position); err != nil {
		return 0, err
	}
	state, err := transaction.FindCurrentSyncDeviceState(ctx)
	if errors.Is(err, domain.ErrSyncDeviceNotFound) {
		return 0, domain.ErrSyncSnapshotExpired
	}
	if err != nil {
		return 0, err
	}
	if err := validateSyncChangeDevice(position.Generation, state); err != nil {
		return 0, err
	}

	// 2. 读取日志区间，复核签名位置并决定是否更新。
	bounds, err := transaction.ReadCurrentSyncChangeSequenceBounds(ctx)
	if err != nil {
		return 0, err
	}
	decision, err := decideSyncChangeConfirmation(position.Sequence, state.AckSequence, bounds)
	if err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	// 3. 按决定返回现值或执行条件更新，并将错误交给外层事务。
	if !decision.RecordActivity {
		return decision.AckSequence, nil
	}
	matched, err := transaction.RecordSyncChangeConfirmation(ctx, state.Generation, state.AckSequence, decision.AckSequence)
	if err != nil {
		return 0, err
	}
	if !matched {
		return 0, domain.ErrInvalidSyncDeviceData
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return decision.AckSequence, nil
}
