package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// CachedSyncSnapshot 是一次读取中已经核对绑定和有效期的快照。
// 仅供当次请求使用，下一次请求需重新通过缓存检查。
type CachedSyncSnapshot struct {
	Header    domain.SyncSnapshotHeader
	ExpiresAt time.Time
	pageSize  int
	items     []json.RawMessage
}

// ItemCount 返回快照中的条目数。
func (snapshot *CachedSyncSnapshot) ItemCount() int {
	return len(snapshot.items)
}

// Item 返回指定位置的独立 JSON 副本，位置超出范围时返回 false。
func (snapshot *CachedSyncSnapshot) Item(index int) (json.RawMessage, bool) {
	if index < 0 || index >= len(snapshot.items) {
		return nil, false
	}
	return bytes.Clone(snapshot.items[index]), true
}

// Get 读取认证账号和设备的缓存快照。Generation 须使用设备当前已持久化的代次。
func (cache *SyncSnapshotCache) Get(ctx context.Context, binding SyncSnapshotCacheBinding) (*CachedSyncSnapshot, error) {
	// 1. 核对参数和可信绑定。归属不匹配时直接拒绝。
	if ctx == nil {
		return nil, fmt.Errorf("读取同步快照缓存需要上下文")
	}
	if !binding.valid() {
		return nil, domain.ErrInvalidSyncSnapshotCursor
	}
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entry, exists := cache.entries[binding.SnapshotID]
	if !exists {
		return nil, domain.ErrSyncSnapshotExpired
	}
	if entry.binding.UserID != binding.UserID || entry.binding.DeviceID != binding.DeviceID || entry.binding.PageSize != binding.PageSize {
		return nil, domain.ErrInvalidSyncSnapshotCursor
	}

	// 2. 核对固定有效期和设备当前代次。只回收已经按时间到期的快照。
	now := cache.now()
	if now.IsZero() {
		return nil, fmt.Errorf("同步快照缓存时钟无效")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !now.Before(entry.expiresAt) {
		cache.removeLocked(binding.SnapshotID, entry)
		return nil, domain.ErrSyncSnapshotExpired
	}
	if entry.binding.Generation != binding.Generation {
		return nil, domain.ErrSyncSnapshotExpired
	}

	// 3. 返回固定头部，以只读方式引用条目，取单条时才复制其字节。
	return &CachedSyncSnapshot{
		Header: entry.snapshot.Header, ExpiresAt: entry.expiresAt,
		pageSize: entry.binding.PageSize, items: entry.snapshot.Entries.Items,
	}, nil
}
