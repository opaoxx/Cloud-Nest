package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// SyncSnapshotContinuation 保存一次续页取得的可信绑定、头部和条目。
type SyncSnapshotContinuation struct {
	Binding   SyncSnapshotCacheBinding
	Header    domain.SyncSnapshotHeader
	ExpiresAt time.Time
	Page      SyncSnapshotPage
}

// SyncSnapshotContinuationService 组合凭据校验、当前代次查询和缓存分页。
type SyncSnapshotContinuationService struct {
	verifyCursor   func(uint64, uint64, uuid.UUID, string, time.Time) (*VerifiedSyncSnapshotCursor, error)
	findGeneration func(context.Context, uint64, uint64) (domain.SyncCursorGeneration, error)
	readCache      func(context.Context, SyncSnapshotCacheBinding) (*CachedSyncSnapshot, error)
	now            func() time.Time
}

// NewSyncSnapshotContinuationService 绑定已有的续页读取能力和时钟。
func NewSyncSnapshotContinuationService(
	verifyCursor func(uint64, uint64, uuid.UUID, string, time.Time) (*VerifiedSyncSnapshotCursor, error),
	findGeneration func(context.Context, uint64, uint64) (domain.SyncCursorGeneration, error),
	readCache func(context.Context, SyncSnapshotCacheBinding) (*CachedSyncSnapshot, error),
	now func() time.Time,
) *SyncSnapshotContinuationService {
	if verifyCursor == nil || findGeneration == nil || readCache == nil || now == nil {
		panic("读取同步快照续页需要凭据校验、代次查询、缓存读取与时钟")
	}
	return &SyncSnapshotContinuationService{verifyCursor: verifyCursor, findGeneration: findGeneration, readCache: readCache, now: now}
}

// Read 用已认证身份读取续页，返回后续签发所需的原快照信息。
func (reader *SyncSnapshotContinuationService) Read(ctx context.Context, actor domain.Actor, snapshotID uuid.UUID, cursor string) (*SyncSnapshotContinuation, error) {
	// 1. 用已认证身份和路径快照 ID 校验续页凭据。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 {
		return nil, fmt.Errorf("读取同步快照续页需要上下文和完整认证身份")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	verified, verifyError := reader.verifyCursor(actor.UserID, actor.DeviceID, snapshotID, cursor, reader.now())
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if verifyError != nil {
		return nil, verifyError
	}
	if verified == nil {
		return nil, fmt.Errorf("同步快照续页校验结果为空")
	}

	// 2. 查询设备当前代次，失配或没有同步记录时提示重建。
	if err := requireCurrentSyncSnapshotGeneration(ctx, actor, verified.Binding.Generation, reader.findGeneration); err != nil {
		return nil, err
	}

	// 3. 通过缓存检查固定绑定和有效期，再按已签名的起点取得一页。
	snapshot, cacheError := reader.readCache(ctx, verified.Binding)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cacheError != nil {
		return nil, cacheError
	}
	if snapshot == nil {
		return nil, fmt.Errorf("同步快照缓存读取结果为空")
	}
	if !snapshot.ExpiresAt.Equal(verified.ExpiresAt) {
		return nil, domain.ErrInvalidSyncSnapshotCursor
	}
	page, err := snapshot.Page(verified.NextPosition)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &SyncSnapshotContinuation{
		Binding: verified.Binding, Header: snapshot.Header, ExpiresAt: snapshot.ExpiresAt, Page: *page,
	}, nil
}

// requireCurrentSyncSnapshotGeneration 确认快照仍属于设备当前已提交的代次。
func requireCurrentSyncSnapshotGeneration(ctx context.Context, actor domain.Actor, expected domain.SyncCursorGeneration,
	findGeneration func(context.Context, uint64, uint64) (domain.SyncCursorGeneration, error),
) error {
	// 1. 按已认证账号和设备读取当前代次，优先检查请求取消。
	generation, err := findGeneration(ctx, actor.UserID, actor.DeviceID)
	if contextError := ctx.Err(); contextError != nil {
		return contextError
	}
	// 2. 无同步记录或代次变化均表示快照失效，保留其他查询错误。
	if errors.Is(err, domain.ErrSyncDeviceNotFound) {
		return domain.ErrSyncSnapshotExpired
	}
	if err != nil {
		return fmt.Errorf("读取设备当前同步代次失败: %w", err)
	}
	if generation != expected {
		return domain.ErrSyncSnapshotExpired
	}
	return nil
}
