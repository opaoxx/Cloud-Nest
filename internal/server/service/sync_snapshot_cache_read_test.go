package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func putSyncSnapshotForReadTest(t *testing.T, cache *SyncSnapshotCache, binding SyncSnapshotCacheBinding, data *BuiltSyncSnapshot) time.Time {
	t.Helper()
	reservation, err := cache.ReserveBuild(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	expiresAt, err := cache.Put(t.Context(), reservation, binding, data)
	if err != nil {
		t.Fatal(err)
	}
	return expiresAt
}

func TestSyncSnapshotCacheGetBindingAndFixedData(t *testing.T) {
	// 1. 读取有效快照，只复制本次取用的条目。
	now := time.Date(2026, 11, 23, 2, 0, 0, 123456000, time.UTC)
	cache := NewSyncSnapshotCache(func() time.Time { return now })
	binding, data := syncSnapshotCacheFixture()
	expiresAt := putSyncSnapshotForReadTest(t, cache, binding, data)
	first, err := cache.Get(t.Context(), binding)
	if err != nil || first == nil || first.Header != data.Header || first.ExpiresAt != expiresAt || first.ItemCount() != 1 {
		t.Fatalf("首次读取头部、期限或条目数错误，错误=%v", err)
	}
	item, ok := first.Item(0)
	if !ok || string(item) != snapshotCacheFixtureItem {
		t.Fatal("首条读取结果错误")
	}
	item[0] = '['
	first.Header.HighWaterSequence++
	for _, index := range []int{-1, first.ItemCount()} {
		if item, ok := first.Item(index); ok || item != nil {
			t.Fatalf("越界位置 %d 应返回空结果", index)
		}
	}

	// 2. 移动时钟后重读，固定数据和到期时间保持原值。
	now = now.Add(5 * time.Minute)
	again, err := cache.Get(t.Context(), binding)
	if err != nil || again == nil || again.Header != data.Header || again.ExpiresAt != expiresAt {
		t.Fatalf("再次读取影响了头部或固定期限，错误=%v", err)
	}
	if sameItem, ok := again.Item(0); !ok || string(sameItem) != snapshotCacheFixtureItem {
		t.Fatal("修改读取结果影响了缓存内容")
	}

	// 3. 核对账号、设备、页大小和代次错误。拟生效的新代次不得被旧代次查询提前清除。
	for _, testCase := range []struct {
		name   string
		mutate func(*SyncSnapshotCacheBinding)
		want   error
	}{
		{"wrong account", func(key *SyncSnapshotCacheBinding) { key.UserID++ }, domain.ErrInvalidSyncSnapshotCursor},
		{"wrong device", func(key *SyncSnapshotCacheBinding) { key.DeviceID++ }, domain.ErrInvalidSyncSnapshotCursor},
		{"wrong page size", func(key *SyncSnapshotCacheBinding) { key.PageSize++ }, domain.ErrInvalidSyncSnapshotCursor},
		{"malformed identifier", func(key *SyncSnapshotCacheBinding) { key.SnapshotID = uuid.Nil }, domain.ErrInvalidSyncSnapshotCursor},
		{"missing", func(key *SyncSnapshotCacheBinding) { key.SnapshotID[15] ^= 1 }, domain.ErrSyncSnapshotExpired},
		{"first generation byte differs", func(key *SyncSnapshotCacheBinding) { key.Generation[0] ^= 1 }, domain.ErrSyncSnapshotExpired},
		{"last generation byte differs", func(key *SyncSnapshotCacheBinding) { key.Generation[15] ^= 1 }, domain.ErrSyncSnapshotExpired},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			changed := binding
			testCase.mutate(&changed)
			if result, err := cache.Get(t.Context(), changed); result != nil || !errors.Is(err, testCase.want) {
				t.Fatalf("读取错误类别不符，错误=%v，预期=%v", err, testCase.want)
			}
			assertSnapshotCacheCapacity(t, cache, 0, 1, data.Entries.SizeBytes)
		})
	}
	if result, err := cache.Get(t.Context(), binding); result == nil || err != nil {
		t.Fatalf("错误绑定影响了有效快照，错误=%v", err)
	}
}

func TestSyncSnapshotCacheGetExpiryBoundary(t *testing.T) {
	// 1. 到期前 1 纳秒仍可读，取消的读取保持缓存不动。
	now := time.Date(2026, 11, 23, 2, 0, 0, 0, time.UTC)
	cache := NewSyncSnapshotCache(func() time.Time { return now })
	binding, data := syncSnapshotCacheFixture()
	expiresAt := putSyncSnapshotForReadTest(t, cache, binding, data)
	now = expiresAt.Add(-time.Nanosecond)
	if result, err := cache.Get(t.Context(), binding); result == nil || err != nil || result.ExpiresAt != expiresAt {
		t.Fatalf("到期之前应可读，错误=%v", err)
	}
	now = expiresAt
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if result, err := cache.Get(canceled, binding); result != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("取消的读取应返回取消错误，错误=%v", err)
	}
	assertSnapshotCacheCapacity(t, cache, 0, 1, data.Entries.SizeBytes)

	// 2. 在到期时刻并发读取，统一拒绝且容量只归还一次。
	const readers = 8
	start := make(chan struct{})
	results := make(chan bool, readers)
	for range readers {
		go func() {
			<-start
			result, err := cache.Get(t.Context(), binding)
			results <- result == nil && errors.Is(err, domain.ErrSyncSnapshotExpired)
		}()
	}
	close(start)
	for range readers {
		if !<-results {
			t.Error("到期时刻不得返回快照")
		}
	}
	if len(cache.entries) != 0 {
		t.Fatal("读取到期快照后应清除缓存")
	}
	assertSnapshotCacheCapacity(t, cache, 0, 0, 0)
	now = expiresAt.Add(time.Nanosecond)
	if result, err := cache.Get(t.Context(), binding); result != nil || !errors.Is(err, domain.ErrSyncSnapshotExpired) {
		t.Fatalf("到期之后必须继续拒绝，错误=%v", err)
	}
}

func TestSyncSnapshotCacheReserveBuildReclaimsExpiredSlots(t *testing.T) {
	// 1. 填满 32 个槽位，让其中 31 个比最后一个先到期。
	now := time.Date(2026, 11, 23, 2, 0, 0, 0, time.UTC)
	cache := NewSyncSnapshotCache(func() time.Time { return now })
	binding, data := syncSnapshotCacheFixture()
	firstExpiry := now.Add(10 * time.Minute)
	for index := range 31 {
		binding.SnapshotID[15] = byte(index)
		putSyncSnapshotForReadTest(t, cache, binding, data)
	}
	now = now.Add(time.Second)
	binding.SnapshotID[15] = 31
	lastExpiry := putSyncSnapshotForReadTest(t, cache, binding, data)
	now = firstExpiry.Add(-time.Nanosecond)
	if reservation, err := cache.ReserveBuild(t.Context()); reservation != nil || !errors.Is(err, domain.ErrSyncSnapshotBusy) {
		t.Fatalf("32 个快照均有效时应拒绝新构建，错误=%v", err)
	}

	// 2. 无需先请求已到期的条目，新的构建申请就会归还它们的预算。
	now = firstExpiry
	reservation, err := cache.ReserveBuild(t.Context())
	if err != nil || reservation == nil || len(cache.entries) != 1 {
		t.Fatalf("未回收无人访问的到期快照，错误=%v", err)
	}
	defer reservation.Release()
	assertSnapshotCacheCapacity(t, cache, 1, 2, domain.MaxSyncSnapshotBytes+data.Entries.SizeBytes)
	if result, err := cache.Get(t.Context(), binding); result == nil || err != nil || result.ExpiresAt != lastExpiry {
		t.Fatalf("回收影响了仍有效的快照，错误=%v", err)
	}
}
