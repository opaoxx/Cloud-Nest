package service

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// SyncSnapshotBaselineConfirmationService 从末页凭据取得已提交的设备确认位置。
type SyncSnapshotBaselineConfirmationService struct {
	verifier        *SyncSnapshotBaselineVerifier
	withTransaction func(context.Context, domain.Actor, func(context.Context, SyncSnapshotBaselineConfirmationTransaction) error) error
	now             func() time.Time
}

// NewSyncSnapshotBaselineConfirmationService 绑定验签、已认证账号事务和时钟。
func NewSyncSnapshotBaselineConfirmationService(
	verifier *SyncSnapshotBaselineVerifier,
	withTransaction func(context.Context, domain.Actor, func(context.Context, SyncSnapshotBaselineConfirmationTransaction) error) error,
	now func() time.Time,
) *SyncSnapshotBaselineConfirmationService {
	if verifier == nil || withTransaction == nil || now == nil {
		panic("确认同步快照基线需要凭据校验器、账号事务和时钟")
	}
	return &SyncSnapshotBaselineConfirmationService{verifier: verifier, withTransaction: withTransaction, now: now}
}

// Confirm 验证原始末页凭据，仅在账号事务提交成功后返回设备的确认序号。
func (confirmer *SyncSnapshotBaselineConfirmationService) Confirm(ctx context.Context, actor domain.Actor, snapshotID uuid.UUID, token string) (uint64, error) {
	// 1. 校验当前身份和凭据绑定，验签失败时不进入账号事务。
	if ctx == nil {
		return 0, fmt.Errorf("确认同步快照基线需要上下文")
	}
	if actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 {
		return 0, domain.ErrInvalidSessionData
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	verified, verifyError := confirmer.verifier.Verify(actor.UserID, actor.DeviceID, snapshotID, token, confirmer.now())
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if verifyError != nil {
		return 0, verifyError
	}

	// 2. 进入同一账号事务，在锁内复核或更新设备的确认位置。
	var position uint64
	confirmed := false
	transactionError := confirmer.withTransaction(ctx, actor, func(txContext context.Context, transaction SyncSnapshotBaselineConfirmationTransaction) error {
		confirmedPosition, err := ConfirmSyncSnapshotBaselineInTransaction(txContext, actor, verified, confirmer.now, transaction)
		if err != nil {
			return err
		}
		position, confirmed = confirmedPosition, true
		return nil
	})

	// 3. 等事务回执。取消、回滚和提交结果未知时均不交付位置。
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if transactionError != nil {
		return 0, transactionError
	}
	if !confirmed {
		return 0, fmt.Errorf("账号事务未完成同步快照基线确认")
	}
	return position, nil
}
