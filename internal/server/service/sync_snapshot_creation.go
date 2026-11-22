package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// SyncSnapshotCreationService 组合快照首次创建和同键重试。
type SyncSnapshotCreationService struct {
	preparation     *SyncSnapshotPreparation
	initialReplay   *SyncSnapshotCreationReplay
	pageReader      *SyncSnapshotReplayService
	withTransaction func(context.Context, domain.Actor, func(context.Context, SyncSnapshotFinalizationTransaction) error) error
	encodeReceipt   func(domain.SyncSnapshotReceiptLocator) (json.RawMessage, error)
	decodeReceipt   func(json.RawMessage) (*domain.SyncSnapshotReceiptLocator, error)
	now             func() time.Time
}

// NewSyncSnapshotCreationService 绑定账号事务和只读代次查询，让准备和交付共用同一缓存。
func NewSyncSnapshotCreationService(
	preparation *SyncSnapshotPreparation,
	withTransaction func(context.Context, domain.Actor, func(context.Context, SyncSnapshotFinalizationTransaction) error) error,
	findGeneration func(context.Context, uint64, uint64) (domain.SyncCursorGeneration, error),
	delivery *SyncSnapshotPageDelivery,
	encodeReceipt func(domain.SyncSnapshotReceiptLocator) (json.RawMessage, error),
	decodeReceipt func(json.RawMessage) (*domain.SyncSnapshotReceiptLocator, error),
	now func() time.Time,
) *SyncSnapshotCreationService {
	if preparation == nil || withTransaction == nil || encodeReceipt == nil {
		panic("创建同步快照需要准备方法、账号事务及回执编码")
	}
	pageReader := NewSyncSnapshotReplayService(findGeneration, preparation.cache.Get, delivery)
	initialReplay := NewSyncSnapshotCreationReplay(
		func(ctx context.Context, actor domain.Actor, action func(context.Context, SyncSnapshotReceiptReader) error) error {
			return withTransaction(ctx, actor, func(txContext context.Context, transaction SyncSnapshotFinalizationTransaction) error {
				return action(txContext, transaction)
			})
		}, decodeReceipt, pageReader, now)
	return &SyncSnapshotCreationService{
		preparation: preparation, initialReplay: initialReplay, pageReader: pageReader, withTransaction: withTransaction,
		encodeReceipt: encodeReceipt, decodeReceipt: decodeReceipt, now: now,
	}
}

// Create 取得已提交快照的第一页，同键请求优先读回原快照。
func (creator *SyncSnapshotCreationService) Create(ctx context.Context, actor domain.Actor, key string, pageSize int) (*domain.SyncSnapshotPageResult, error) {
	// 1. 先查已有回执，命中则重读原首页。
	page, err := creator.initialReplay.ReadExistingFirstPage(ctx, actor, key, pageSize)
	if err != nil || page != nil {
		return page, err
	}

	// 2. 准备候选快照，在账号事务中做最终复查和写入。
	prepared, err := creator.preparation.Prepare(ctx, actor, pageSize)
	if err != nil {
		return nil, err
	}
	keepPrepared := false
	defer func() {
		if !keepPrepared {
			creator.preparation.cache.DiscardUncommitted(prepared.Binding, prepared.reservation)
		}
	}()
	var existing *domain.SyncSnapshotReceiptLocator
	finalized := false
	transactionError := creator.withTransaction(ctx, actor, func(txContext context.Context, transaction SyncSnapshotFinalizationTransaction) error {
		locator, err := FinalizeSyncSnapshotCreationInTransaction(
			txContext, actor, key, prepared, creator.now(), transaction, creator.encodeReceipt, creator.decodeReceipt,
		)
		if err != nil {
			return err
		}
		existing, finalized = locator, true
		return nil
	})

	// 3. 按事务结果处理暂存缓存，从已提交代次交付第一页。
	// 提交结果未知时，原缓存仍可在同键重试中通过已提交代次验证。
	keepPrepared = finalized && existing == nil &&
		(transactionError == nil || errors.Is(transactionError, domain.ErrUserTransactionCommitUnconfirmed))
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if transactionError != nil {
		return nil, transactionError
	}
	if !finalized {
		return nil, fmt.Errorf("账号事务未完成同步快照创建")
	}
	if existing != nil {
		return creator.pageReader.ReadFirstPage(ctx, actor, *existing, pageSize)
	}
	locator := domain.SyncSnapshotReceiptLocator{
		SnapshotID: prepared.Binding.SnapshotID, DeviceID: prepared.Binding.DeviceID, Generation: prepared.Binding.Generation,
	}
	return creator.pageReader.ReadFirstPage(ctx, actor, locator, pageSize)
}
