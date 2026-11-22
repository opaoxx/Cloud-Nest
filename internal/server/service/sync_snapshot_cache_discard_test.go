package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func putDiscardableSyncSnapshot(t *testing.T, ctx context.Context, cache *SyncSnapshotCache,
	binding SyncSnapshotCacheBinding, data *BuiltSyncSnapshot,
) *SyncSnapshotReservation {
	t.Helper()
	reservation, err := cache.ReserveBuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Put(ctx, reservation, binding, data); err != nil {
		t.Fatal(err)
	}
	return reservation
}

func TestSyncSnapshotCacheDiscardUncommittedBinding(t *testing.T) {
	// 1. 同时缓存两个快照，保留待撤回项的原预留。
	stamp := time.Date(2026, 11, 23, 2, 0, 0, 0, time.UTC)
	cache := NewSyncSnapshotCache(func() time.Time { return stamp })
	binding, data := syncSnapshotCacheFixture()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reservation := putDiscardableSyncSnapshot(t, ctx, cache, binding, data)
	otherBinding := binding
	otherBinding.SnapshotID[15] ^= 1
	otherReservation := putDiscardableSyncSnapshot(t, t.Context(), cache, otherBinding, data)
	assertSnapshotCacheCapacity(t, cache, 0, 2, 2*data.Entries.SizeBytes)

	// 2. 逐项改变绑定字段或预留，均不能撤回目标快照。
	for _, testCase := range []struct {
		name   string
		change func(*SyncSnapshotCacheBinding)
	}{
		{"snapshot ID", func(value *SyncSnapshotCacheBinding) { value.SnapshotID[15] ^= 2 }},
		{"user", func(value *SyncSnapshotCacheBinding) { value.UserID++ }},
		{"device", func(value *SyncSnapshotCacheBinding) { value.DeviceID++ }},
		{"generation", func(value *SyncSnapshotCacheBinding) { value.Generation[15] ^= 1 }},
		{"page size", func(value *SyncSnapshotCacheBinding) { value.PageSize++ }},
		{"invalid page size", func(value *SyncSnapshotCacheBinding) { value.PageSize = 0 }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			mismatch := binding
			testCase.change(&mismatch)
			if cache.DiscardUncommitted(mismatch, reservation) {
				t.Fatal("绑定失配后仍撤回快照")
			}
		})
	}
	for _, wrongReservation := range []*SyncSnapshotReservation{nil, {}, otherReservation} {
		if cache.DiscardUncommitted(binding, wrongReservation) {
			t.Fatal("预留失配后仍撤回快照")
		}
	}
	assertSnapshotCacheCapacity(t, cache, 0, 2, 2*data.Entries.SizeBytes)

	// 3. 请求取消后仍可撤回一次，预算释放不涉及其他快照。
	cancel()
	copyOfReservation := *reservation
	if !cache.DiscardUncommitted(binding, &copyOfReservation) || cache.DiscardUncommitted(binding, reservation) {
		t.Fatal("同一次构建应当恰好撤回一次")
	}
	assertSnapshotCacheCapacity(t, cache, 0, 1, data.Entries.SizeBytes)
	if view, err := cache.Get(t.Context(), binding); view != nil || !errors.Is(err, domain.ErrSyncSnapshotExpired) {
		t.Fatalf("撤回后仍能读取：%+v, %v", view, err)
	}
	if view, err := cache.Get(t.Context(), otherBinding); err != nil || view == nil || view.ItemCount() != 1 {
		t.Fatalf("另一快照受到影响：%+v, %v", view, err)
	}

	// 4. 即使完整绑定被复用，迟到的旧预留也不能撤回新入缓存的项。
	replacement := putDiscardableSyncSnapshot(t, t.Context(), cache, binding, data)
	if cache.DiscardUncommitted(binding, reservation) || !cache.DiscardUncommitted(binding, replacement) {
		t.Fatal("复用绑定后未按新的构建预留确认归属")
	}
	assertSnapshotCacheCapacity(t, cache, 0, 1, data.Entries.SizeBytes)
}

func TestSyncSnapshotCacheDiscardUncommittedConcurrent(t *testing.T) {
	for _, expires := range []bool{false, true} {
		name := "concurrent discard"
		if expires {
			name = "discard with expiry"
		}
		t.Run(name, func(t *testing.T) {
			// 1. 固定相同时刻，并在到期场景让正常读取与撤回竞争。
			now := time.Date(2026, 11, 23, 2, 0, 0, 0, time.UTC)
			cache := NewSyncSnapshotCache(func() time.Time { return now })
			binding, data := syncSnapshotCacheFixture()
			ctx := t.Context()
			reservation := putDiscardableSyncSnapshot(t, ctx, cache, binding, data)
			var callers sync.WaitGroup
			var discarded atomic.Int32
			start := make(chan struct{})
			if expires {
				now = now.Add(syncSnapshotCacheTTL)
				callers.Go(func() {
					<-start
					if view, err := cache.Get(ctx, binding); view != nil || !errors.Is(err, domain.ErrSyncSnapshotExpired) {
						t.Errorf("到期读取返回 %+v，错误 %v", view, err)
					}
				})
			}
			for range 24 {
				callers.Go(func() {
					<-start
					if cache.DiscardUncommitted(binding, reservation) {
						discarded.Add(1)
					}
				})
			}

			// 2. 并发结束后只有一种路径能移除缓存项，预算恰好归零。
			close(start)
			callers.Wait()
			if count := discarded.Load(); count > 1 || (!expires && count != 1) || len(cache.entries) != 0 {
				t.Fatalf("撤回次数为 %d，剩余缓存为 %d", count, len(cache.entries))
			}
			assertSnapshotCacheCapacity(t, cache, 0, 0, 0)
		})
	}
}
