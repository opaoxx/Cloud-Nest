package service

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// PurgeEntryTransaction 提供同一用户事务中的预检读取与条件写入。
type PurgeEntryTransaction interface {
	PurgePreviewReader
	// BeginUserEntryPurge 按预期版本标记目标，提交由外层事务负责。
	BeginUserEntryPurge(ctx context.Context, entryID, expectedRevision uint64) (*domain.FileEntry, error)
}

// PurgeEntryService 组合首次清理的范围复查和状态更新。
type PurgeEntryService struct {
	// withTransaction 同步执行一次回调，确认提交后才返回 nil。
	withTransaction func(context.Context, domain.Actor, func(context.Context, PurgeEntryTransaction) error) error
	verifier        *PurgePreviewVerifier
	now             func() time.Time
}

// NewPurgeEntryService 绑定已有用户事务、凭证校验器和应用时钟。
func NewPurgeEntryService(
	withTransaction func(context.Context, domain.Actor, func(context.Context, PurgeEntryTransaction) error) error,
	verifier *PurgePreviewVerifier,
	now func() time.Time,
) *PurgeEntryService {
	// 1. 检查并保存首次受理所需的依赖。
	if withTransaction == nil || verifier == nil || now == nil {
		panic("永久清理受理需要已认证用户事务、凭证校验器和时钟")
	}
	return &PurgeEntryService{withTransaction: withTransaction, verifier: verifier, now: now}
}

// Begin 首次受理单项永久清理，事务提交后返回目标。
func (purgeService *PurgeEntryService) Begin(ctx context.Context, actor domain.Actor, entryID, expectedRevision uint64, digest string) (*domain.FileEntry, error) {
	// 1. 校验输入，进入已认证用户事务。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || entryID == 0 || expectedRevision == 0 {
		return nil, fmt.Errorf("永久清理受理需要上下文、完整认证身份、非零目标和预期版本")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !domain.ValidPurgePreviewDigestFormat(digest) {
		return nil, domain.ErrInvalidPurgePreviewDigest
	}

	var prepared *domain.FileEntry
	transactionError := purgeService.withTransaction(ctx, actor, func(txContext context.Context, txStore PurgeEntryTransaction) error {
		// 2. 在事务内复查并写入 PURGING。
		var err error
		prepared, err = beginPurgeEntryInTransaction(txContext, actor, entryID, expectedRevision, digest, txStore, purgeService.verifier, purgeService.now)
		return err
	})

	// 3. 确认事务提交后返回结果。
	if transactionError != nil {
		return nil, fmt.Errorf("永久清理受理事务失败: %w", transactionError)
	}
	if prepared == nil {
		return nil, domain.ErrInvalidFileEntryData
	}
	return prepared, nil
}

// beginPurgeEntryInTransaction 执行首次受理，调用方持有用户锁并负责提交。
func beginPurgeEntryInTransaction(
	ctx context.Context,
	actor domain.Actor,
	entryID, expectedRevision uint64,
	digest string,
	txStore PurgeEntryTransaction,
	verifier *PurgePreviewVerifier,
	now func() time.Time,
) (*domain.FileEntry, error) {
	// 1. 复查预检范围，再比较当前版本与预期版本。
	target, err := RecheckPurgePreview(ctx, actor, entryID, digest, txStore, verifier, now)
	if err != nil {
		return nil, err
	}
	if err := ValidateEntryRevision(target.Revision, expectedRevision); err != nil {
		return nil, err
	}
	if target.Revision == math.MaxUint64 {
		return nil, domain.ErrEntryRevisionExhausted
	}

	// 2. 写入 PURGING，核对写回的目标和版本。
	saved, writeError := txStore.BeginUserEntryPurge(ctx, target.ID, expectedRevision)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if writeError != nil {
		return nil, fmt.Errorf("写入永久清理受理状态失败: %w", writeError)
	}
	if saved == nil || saved.ID != target.ID || saved.UserID != target.UserID ||
		saved.ParentID == nil || *saved.ParentID != *target.ParentID || saved.Name != target.Name ||
		saved.EntryType != target.EntryType || saved.FileSize != target.FileSize ||
		saved.Status != "PURGING" || saved.Revision != expectedRevision+1 ||
		saved.CreatedAt.IsZero() || !saved.CreatedAt.Equal(target.CreatedAt) {
		return nil, domain.ErrInvalidFileEntryData
	}
	startedAt := saved.ModifiedAt.UTC()
	if startedAt.Year() < 1000 || startedAt.Year() > 9999 || startedAt.Nanosecond()%1000 != 0 || !saved.UpdatedAt.Equal(startedAt) {
		return nil, domain.ErrInvalidFileEntryData
	}

	// 3. 复制目录项与可空字段，供当前事务继续处理。
	result := *saved
	result.ParentID = copyOptionalValue(saved.ParentID)
	result.ModifiedAt, result.UpdatedAt = startedAt, startedAt
	return &result, nil
}
