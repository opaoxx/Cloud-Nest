package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// SyncSnapshotReplayService 从有效回执定位的缓存重读快照第一页。
type SyncSnapshotReplayService struct {
	findGeneration func(context.Context, uint64, uint64) (domain.SyncCursorGeneration, error)
	readCache      func(context.Context, SyncSnapshotCacheBinding) (*CachedSyncSnapshot, error)
	delivery       *SyncSnapshotPageDelivery
}

// NewSyncSnapshotReplayService 绑定代次查询、共享缓存读取和分页交付能力。
func NewSyncSnapshotReplayService(
	findGeneration func(context.Context, uint64, uint64) (domain.SyncCursorGeneration, error),
	readCache func(context.Context, SyncSnapshotCacheBinding) (*CachedSyncSnapshot, error),
	delivery *SyncSnapshotPageDelivery,
) *SyncSnapshotReplayService {
	if findGeneration == nil || readCache == nil || delivery == nil {
		panic("重读同步快照第一页需要代次查询、缓存读取和分页交付")
	}
	return &SyncSnapshotReplayService{findGeneration: findGeneration, readCache: readCache, delivery: delivery}
}

// ReadFirstPage 使用已复核的定位数据和请求摘要匹配的页大小重读原页。
func (reader *SyncSnapshotReplayService) ReadFirstPage(ctx context.Context, actor domain.Actor, locator domain.SyncSnapshotReceiptLocator, pageSize int) (*domain.SyncSnapshotPageResult, error) {
	// 1. 核对输入并组装原快照的缓存绑定。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 {
		return nil, fmt.Errorf("重读同步快照第一页需要上下文和完整认证身份")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	binding := SyncSnapshotCacheBinding{
		SnapshotID: locator.SnapshotID, UserID: actor.UserID, DeviceID: actor.DeviceID,
		Generation: locator.Generation, PageSize: pageSize,
	}
	if locator.DeviceID != actor.DeviceID || !binding.valid() {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}

	// 2. 复查设备当前已提交的代次，失效时停止读取。
	if err := requireCurrentSyncSnapshotGeneration(ctx, actor, binding.Generation, reader.findGeneration); err != nil {
		return nil, err
	}

	// 3. 读取位置 0，沿用缓存的原到期时间交付第一页。
	snapshot, cacheError := reader.readCache(ctx, binding)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if errors.Is(cacheError, domain.ErrInvalidSyncSnapshotCursor) {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	if cacheError != nil {
		return nil, cacheError
	}
	if snapshot == nil {
		return nil, fmt.Errorf("重读同步快照第一页时缓存读取结果为空")
	}
	page, err := snapshot.Page(0)
	if err != nil {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	return reader.delivery.Build(ctx, binding, snapshot.Header, snapshot.ExpiresAt, page)
}
