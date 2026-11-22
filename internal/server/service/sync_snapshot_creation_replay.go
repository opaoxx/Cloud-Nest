package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// SyncSnapshotReceiptReader 提供已认证账号事务内的幂等记录查询。
type SyncSnapshotReceiptReader interface {
	FindUserIdempotencyRecord(context.Context, uint64, string, string) (*domain.IdempotencyRecord, error)
}

// SyncSnapshotCreationReplay 在开始构建之前识别并交付已有快照。
type SyncSnapshotCreationReplay struct {
	withTransaction func(context.Context, domain.Actor, func(context.Context, SyncSnapshotReceiptReader) error) error
	decodeReceipt   func(json.RawMessage) (*domain.SyncSnapshotReceiptLocator, error)
	pageReader      *SyncSnapshotReplayService
	now             func() time.Time
}

// NewSyncSnapshotCreationReplay 绑定账号事务、回执解码与事务外的首页读取。
func NewSyncSnapshotCreationReplay(
	withTransaction func(context.Context, domain.Actor, func(context.Context, SyncSnapshotReceiptReader) error) error,
	decodeReceipt func(json.RawMessage) (*domain.SyncSnapshotReceiptLocator, error),
	pageReader *SyncSnapshotReplayService, now func() time.Time,
) *SyncSnapshotCreationReplay {
	if withTransaction == nil || decodeReceipt == nil || pageReader == nil || now == nil {
		panic("重试同步快照创建需要账号事务、回执解码、首页读取与时钟")
	}
	return &SyncSnapshotCreationReplay{
		withTransaction: withTransaction, decodeReceipt: decodeReceipt, pageReader: pageReader, now: now,
	}
}

// ReadExistingFirstPage 尝试用同键回执重读原快照。明确查无记录时返回 nil, nil。
func (replay *SyncSnapshotCreationReplay) ReadExistingFirstPage(ctx context.Context, actor domain.Actor, key string, pageSize int) (*domain.SyncSnapshotPageResult, error) {
	// 1. 校验创建参数，计算本次请求摘要。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || !domain.ValidIdempotencyKey(key) {
		return nil, fmt.Errorf("重试同步快照创建需要上下文、完整认证身份和有效幂等键")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	requestHash, err := BuildCreateSyncSnapshotRequestHash(actor.DeviceID, pageSize)
	if err != nil {
		return nil, err
	}

	// 2. 在账号事务中查找并复核已有回执，以取得账号锁后的时间判断到期。
	var locator *domain.SyncSnapshotReceiptLocator
	lookupCompleted := false
	transactionError := replay.withTransaction(ctx, actor, func(txContext context.Context, txStore SyncSnapshotReceiptReader) error {
		if txStore == nil {
			return fmt.Errorf("查找同步快照创建回执需要账号事务 Store")
		}
		found, err := FindCreateSyncSnapshotReplayInTransaction(
			txContext, actor, key, requestHash, replay.now(), txStore.FindUserIdempotencyRecord, replay.decodeReceipt,
		)
		if err != nil {
			return err
		}
		locator, lookupCompleted = found, true
		return nil
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if transactionError != nil {
		return nil, transactionError
	}
	if !lookupCompleted {
		return nil, fmt.Errorf("账号事务未完成同步快照创建回执查找")
	}

	// 3. 事务结束后，有回执则用原请求上下文重读第一页。
	if locator == nil {
		return nil, nil
	}
	return replay.pageReader.ReadFirstPage(ctx, actor, *locator, pageSize)
}
