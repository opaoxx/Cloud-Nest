package service

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// SyncSnapshotReader 在同一读取视图中取得已校验的活动条目。
type SyncSnapshotReader interface {
	ListSyncSnapshotEntries(context.Context) ([]domain.SyncSnapshotEntry, error)
}

// BuiltSyncSnapshot 是只读构建完成、可供后续缓存使用的快照数据。
type BuiltSyncSnapshot struct {
	Header  domain.SyncSnapshotHeader
	Entries domain.SyncSnapshotEntriesJSON
}

// SyncSnapshotBuilder 组合账号快照读取和条目编码。
type SyncSnapshotBuilder struct {
	// withSnapshot 同步执行读取回调，只在事务成功结束后返回 nil。
	withSnapshot func(context.Context, uint64, func(context.Context, SyncSnapshotReader, domain.SyncSnapshotHeader) error) error
	encode       func([]domain.SyncSnapshotEntry) (*domain.SyncSnapshotEntriesJSON, error)
}

// NewSyncSnapshotBuilder 绑定只读事务及条目编码能力。
func NewSyncSnapshotBuilder(
	withSnapshot func(context.Context, uint64, func(context.Context, SyncSnapshotReader, domain.SyncSnapshotHeader) error) error,
	encode func([]domain.SyncSnapshotEntry) (*domain.SyncSnapshotEntriesJSON, error),
) *SyncSnapshotBuilder {
	if withSnapshot == nil || encode == nil {
		panic("构建同步快照需要只读事务与条目编码方法")
	}
	return &SyncSnapshotBuilder{withSnapshot: withSnapshot, encode: encode}
}

// Build 使用可信身份读取账号快照，在只读事务和编码均成功后返回完整结果。
func (builder *SyncSnapshotBuilder) Build(ctx context.Context, actor domain.Actor) (*BuiltSyncSnapshot, error) {
	// 1. 核对认证身份和取消状态，按账号开启只读快照。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 {
		return nil, fmt.Errorf("构建同步快照需要上下文和完整认证身份")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var header domain.SyncSnapshotHeader
	var entries []domain.SyncSnapshotEntry

	// 2. 从同一视图取得头部和条目，只在事务成功结束后使用读取结果。
	snapshotError := builder.withSnapshot(ctx, actor.UserID, func(readContext context.Context, reader SyncSnapshotReader, snapshotHeader domain.SyncSnapshotHeader) error {
		if readContext == nil || reader == nil || snapshotHeader.RootEntryID == 0 {
			return domain.ErrInvalidAccountData
		}
		readEntries, err := reader.ListSyncSnapshotEntries(readContext)
		if err != nil {
			return err
		}
		header, entries = snapshotHeader, readEntries
		return readContext.Err()
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if snapshotError != nil {
		return nil, fmt.Errorf("读取同步快照失败: %w", snapshotError)
	}
	if header.RootEntryID == 0 {
		return nil, domain.ErrInvalidAccountData
	}

	// 3. 关闭事务后再编码并检查容量，成功才交付整份快照。
	encoded, encodeError := builder.encode(entries)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if encodeError != nil {
		return nil, fmt.Errorf("编码同步快照失败: %w", encodeError)
	}
	if encoded == nil {
		return nil, domain.ErrInvalidFileEntryData
	}
	return &BuiltSyncSnapshot{Header: header, Entries: *encoded}, nil
}
