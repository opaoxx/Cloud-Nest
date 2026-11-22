package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const syncSnapshotCacheTTL = 10 * time.Minute

// SyncSnapshotCacheBinding 固定一份快照的标识、设备代次和分页大小。
type SyncSnapshotCacheBinding struct {
	SnapshotID uuid.UUID
	UserID     uint64
	DeviceID   uint64
	Generation uint64
	PageSize   int
}

func (binding SyncSnapshotCacheBinding) valid() bool {
	return binding.SnapshotID.Version() == 4 && binding.SnapshotID.Variant() == uuid.RFC4122 &&
		binding.UserID != 0 && binding.DeviceID != 0 && binding.Generation != 0 && binding.PageSize >= 1 && binding.PageSize <= 100
}

// SyncSnapshotCache 持有进程内的固定快照及其共享预算。
type SyncSnapshotCache struct {
	mutex    sync.Mutex
	capacity SyncSnapshotCapacity
	now      func() time.Time
	entries  map[uuid.UUID]cachedSyncSnapshot
}

type cachedSyncSnapshot struct {
	binding     SyncSnapshotCacheBinding
	snapshot    BuiltSyncSnapshot
	expiresAt   time.Time
	reservation *SyncSnapshotReservation
}

// NewSyncSnapshotCache 创建一份进程共享的快照缓存。
func NewSyncSnapshotCache(now func() time.Time) *SyncSnapshotCache {
	if now == nil {
		panic("同步快照缓存需要时钟")
	}
	return &SyncSnapshotCache{now: now, entries: make(map[uuid.UUID]cachedSyncSnapshot)}
}

// ReserveBuild 为即将开始的快照构建预留本缓存的名额和空间。
func (cache *SyncSnapshotCache) ReserveBuild(ctx context.Context) (*SyncSnapshotReservation, error) {
	// 1. 检查本次申请，串行处理缓存回收和名额预留。
	if ctx == nil {
		return nil, fmt.Errorf("预留同步快照需要上下文")
	}
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 2. 先回收已到期的快照，再申请名额，避免无人访问的旧项一直占用预算。
	if len(cache.entries) > 0 {
		now := cache.now()
		for snapshotID, entry := range cache.entries {
			if !now.Before(entry.expiresAt) {
				cache.removeLocked(snapshotID, entry)
			}
		}
	}
	return cache.capacity.ReserveBuild(ctx)
}

func (cache *SyncSnapshotCache) removeLocked(snapshotID uuid.UUID, entry cachedSyncSnapshot) {
	delete(cache.entries, snapshotID)
	entry.reservation.Release()
}

// Put 接管本次构建预留并保存条目的独立副本，返回固定的 UTC 到期时刻。
// 成功时释放权移交给缓存，失败时归还传入的预留。
func (cache *SyncSnapshotCache) Put(ctx context.Context, reservation *SyncSnapshotReservation, binding SyncSnapshotCacheBinding, snapshot *BuiltSyncSnapshot) (time.Time, error) {
	// 1. 核对标识、设备绑定、上下文和实际 JSON 大小。
	defer reservation.Release()
	if ctx == nil || !binding.valid() {
		return time.Time{}, fmt.Errorf("缓存同步快照需要上下文和有效的快照绑定")
	}
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	if err := validateSyncSnapshotCacheData(snapshot); err != nil {
		return time.Time{}, err
	}

	// 2. 检查标识未被占用，转交预留并复制条目的 JSON 字节。
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	if _, exists := cache.entries[binding.SnapshotID]; exists {
		return time.Time{}, fmt.Errorf("同步快照标识已被占用")
	}
	retained, err := cache.capacity.retainBuild(reservation, snapshot.Entries.SizeBytes)
	if err != nil {
		return time.Time{}, err
	}
	stored := false
	defer func() {
		if !stored {
			retained.Release()
		}
	}()
	staged := *snapshot
	staged.Entries.Items = make([]json.RawMessage, len(snapshot.Entries.Items))
	for index, item := range snapshot.Entries.Items {
		staged.Entries.Items[index] = bytes.Clone(item)
	}

	// 3. 用入缓存时刻设置固定有效期，返回前确认调用方仍在等待。
	now := cache.now().UTC()
	if now.IsZero() {
		return time.Time{}, fmt.Errorf("同步快照缓存时钟无效")
	}
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	expiresAt := now.Add(syncSnapshotCacheTTL)
	cache.entries[binding.SnapshotID] = cachedSyncSnapshot{binding: binding, snapshot: staged, expiresAt: expiresAt, reservation: retained}
	stored = true
	return expiresAt, nil
}

func validateSyncSnapshotCacheData(snapshot *BuiltSyncSnapshot) error {
	// 1. 核对已构建头部和条目数，空树必须保留空数组。
	if snapshot == nil || snapshot.Header.RootEntryID == 0 || snapshot.Entries.Items == nil {
		return domain.ErrInvalidFileEntryData
	}
	if len(snapshot.Entries.Items) > domain.MaxSyncSnapshotEntries || snapshot.Entries.SizeBytes > domain.MaxSyncSnapshotBytes {
		return domain.ErrSyncSnapshotTooLarge
	}

	// 2. 按数组实际内容复算字节，防止容量结算依赖错误的大小字段。
	sizeBytes := len("[]")
	for index, item := range snapshot.Entries.Items {
		if index > 0 {
			sizeBytes++
		}
		if len(item) > domain.MaxSyncSnapshotBytes-sizeBytes {
			return domain.ErrSyncSnapshotTooLarge
		}
		if len(item) < 2 || item[0] != '{' || item[len(item)-1] != '}' || !json.Valid(item) {
			return domain.ErrInvalidFileEntryData
		}
		sizeBytes += len(item)
	}
	if sizeBytes != snapshot.Entries.SizeBytes {
		return domain.ErrInvalidFileEntryData
	}
	return nil
}
