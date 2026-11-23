package service

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// SyncChangeConfirmationService 验签并返回已提交的设备增量确认位置。
type SyncChangeConfirmationService struct {
	verifier        *SyncChangeCursorVerifier
	withTransaction func(context.Context, domain.Actor, func(context.Context, SyncChangeConfirmationTransaction) error) error
}

// NewSyncChangeConfirmationService 绑定增量游标验签器和认证账号事务。
func NewSyncChangeConfirmationService(
	verifier *SyncChangeCursorVerifier,
	withTransaction func(context.Context, domain.Actor, func(context.Context, SyncChangeConfirmationTransaction) error) error,
) *SyncChangeConfirmationService {
	if verifier == nil || withTransaction == nil {
		panic("增量确认需要游标验签器和账号事务")
	}
	return &SyncChangeConfirmationService{verifier: verifier, withTransaction: withTransaction}
}

// Confirm 只交付账号事务成功提交后确认的位置。
func (confirmer *SyncChangeConfirmationService) Confirm(ctx context.Context, actor domain.Actor, cursor string) (uint64, error) {
	// 1. 检查请求状态和认证身份，再验证游标签名。
	if ctx == nil {
		return 0, fmt.Errorf("确认增量变更需要请求上下文")
	}
	if actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 {
		return 0, domain.ErrInvalidSessionData
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	verified, verifyError := confirmer.verifier.Verify(actor.UserID, actor.DeviceID, cursor)
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if verifyError != nil {
		return 0, verifyError
	}

	// 2. 开启账号事务，调用事务内确认方法。
	var position uint64
	confirmed := false
	transactionError := confirmer.withTransaction(ctx, actor, func(txContext context.Context, transaction SyncChangeConfirmationTransaction) error {
		confirmedPosition, err := ConfirmSyncChangesInTransaction(txContext, actor, verified, transaction)
		if err != nil {
			return err
		}
		position, confirmed = confirmedPosition, true
		return nil
	})

	// 3. 等待事务提交成功，再返回确认位置。
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if transactionError != nil {
		return 0, transactionError
	}
	if !confirmed {
		return 0, fmt.Errorf("账号事务未完成增量确认")
	}
	return position, nil
}
