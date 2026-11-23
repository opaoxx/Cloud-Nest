package service

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// SyncChangePageReader 提供已绑定账号的只读事务查询。
type SyncChangePageReader interface {
	ReadSyncChangeSequenceBounds(context.Context) (domain.SyncChangeSequenceBounds, error)
	ListSyncChangeEvents(ctx context.Context, position, highWaterSequence uint64, pageSize int) ([]domain.SyncChangeEvent, error)
}

// SyncChangeReadPage 保存同一只读视图中已经通过校验的事件页。
type SyncChangeReadPage struct {
	HighWaterSequence uint64
	Events            []domain.SyncChangeEvent
	startPosition     domain.SyncChangeCursorPosition
	pageSize          int
}

// ReadSyncChangePageInSnapshot 在拉取读视图中复核起点并读取整页。
// state、ctx 和 reader 须来自同一个事务回调，调用方在事务成功结束后才可使用结果。
func ReadSyncChangePageInSnapshot(
	ctx context.Context, actor domain.Actor, start *SyncChangePullStart, state domain.SyncDeviceState,
	pageSize int, now func() time.Time, reader SyncChangePageReader,
) (*SyncChangeReadPage, error) {
	// 1. 读取同一事务中的日志区间，再复核已验签的起点。
	if ctx == nil || start == nil || now == nil || reader == nil {
		return nil, fmt.Errorf("读取增量页需要上下文、已验签起点、时钟和只读视图")
	}
	if pageSize < 1 || pageSize > 100 {
		return nil, fmt.Errorf("%w: 页大小 %d 无效", domain.ErrInvalidSyncChangePage, pageSize)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bounds, err := reader.ReadSyncChangeSequenceBounds(ctx)
	if err != nil {
		return nil, err
	}
	if err := ValidateSyncChangePullStart(actor, start, state, bounds, now()); err != nil {
		return nil, err
	}

	// 2. 用复核后的起点和上界限量查询事件。
	startPosition := start.Position
	position := startPosition.Sequence
	events, err := reader.ListSyncChangeEvents(ctx, position, bounds.HighWaterSequence, pageSize)
	if err != nil {
		return nil, err
	}

	// 3. 校验整页并检查取消，成功才交回结果。
	if err := ValidateSyncChangePage(position, bounds.HighWaterSequence, pageSize, events); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &SyncChangeReadPage{
		HighWaterSequence: bounds.HighWaterSequence,
		Events:            events,
		startPosition:     startPosition,
		pageSize:          pageSize,
	}, nil
}
