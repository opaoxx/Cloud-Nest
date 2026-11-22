package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const snapshotCacheFixtureItem = `{"entry_id":"12","parent_id":"11","name":"资料\u0026目录","entry_type":"DIR","revision":"1","size_bytes":0,"sha256":null}`

func syncSnapshotCacheFixture() (SyncSnapshotCacheBinding, *BuiltSyncSnapshot) {
	return SyncSnapshotCacheBinding{
		SnapshotID: uuid.MustParse("052dc4f7-f4c1-46b3-a509-bf96992f6580"),
		UserID:     7, DeviceID: 19, Generation: 3, PageSize: 50,
	}, &BuiltSyncSnapshot{
		Header: domain.SyncSnapshotHeader{RootEntryID: 11, HighWaterSequence: 42},
		Entries: domain.SyncSnapshotEntriesJSON{
			Items: []json.RawMessage{json.RawMessage(snapshotCacheFixtureItem)}, SizeBytes: len(snapshotCacheFixtureItem) + 2,
		},
	}
}

func assertSnapshotCacheCapacity(t *testing.T, cache *SyncSnapshotCache, builds, slots, sizeBytes int) {
	t.Helper()
	if got := &cache.capacity; got.activeBuilds != builds || got.usedSlots != slots || got.usedBytes != sizeBytes {
		t.Fatalf("容量实际为构建 %d、槽位 %d、字节 %d，预期 %d、%d、%d", got.activeBuilds, got.usedSlots, got.usedBytes, builds, slots, sizeBytes)
	}
}

func TestSyncSnapshotCachePutRetainsImmutableData(t *testing.T) {
	// 1. 使用固定时钟写入一份快照，核对元数据、到期时间和实际结算。
	stamp := time.Date(2026, 11, 23, 9, 30, 0, 123456000, time.FixedZone("UTC+8", 8*60*60))
	cache := NewSyncSnapshotCache(func() time.Time { return stamp })
	binding, snapshot := syncSnapshotCacheFixture()
	wantHeader := snapshot.Header
	wantSize := snapshot.Entries.SizeBytes
	reservation, err := cache.ReserveBuild(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer reservation.Release()
	copyOfReservation := *reservation
	assertSnapshotCacheCapacity(t, cache, 1, 1, domain.MaxSyncSnapshotBytes)
	expiresAt, err := cache.Put(t.Context(), reservation, binding, snapshot)
	wantExpiry := stamp.UTC().Add(10 * time.Minute)
	if err != nil || expiresAt != wantExpiry || len(cache.entries) != 1 {
		t.Fatalf("快照写入或有效期错误，到期=%s，错误=%v", expiresAt, err)
	}
	assertSnapshotCacheCapacity(t, cache, 0, 1, wantSize)

	// 2. 修改原输入并释放旧凭据，缓存的字节与归属仍可完整保留。
	snapshot.Header.HighWaterSequence = 43
	snapshot.Entries.Items[0][0] = '['
	snapshot.Entries.Items[0] = nil
	snapshot.Entries.SizeBytes = 2
	reservation.Release()
	copyOfReservation.Release()
	assertSnapshotCacheCapacity(t, cache, 0, 1, wantSize)
	stored := cache.entries[binding.SnapshotID]
	if stored.binding != binding || stored.snapshot.Header != wantHeader || stored.expiresAt != wantExpiry ||
		stored.snapshot.Entries.SizeBytes != wantSize || string(stored.snapshot.Entries.Items[0]) != snapshotCacheFixtureItem {
		t.Fatal("缓存数据被原输入修改，或写入后丢失元数据")
	}

	// 3. 时钟向前移动后重复写入相同标识，原内容和原到期时刻继续保留。
	stamp = stamp.Add(time.Minute)
	_, duplicate := syncSnapshotCacheFixture()
	duplicate.Header.HighWaterSequence++
	secondReservation, err := cache.ReserveBuild(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if expiry, err := cache.Put(t.Context(), secondReservation, binding, duplicate); err == nil || !expiry.IsZero() {
		t.Fatal("重复写入不应成功")
	}
	if current := cache.entries[binding.SnapshotID]; current.expiresAt != wantExpiry || current.snapshot.Header != wantHeader {
		t.Fatal("重复写入覆盖了原快照")
	}
	assertSnapshotCacheCapacity(t, cache, 0, 1, wantSize)
}

func TestSyncSnapshotCachePutReleasesFailedBuild(t *testing.T) {
	// 1. 逐项注入写入错误，包括转交预留前和转交之后两个阶段。
	for _, testCase := range []struct {
		name   string
		mutate func(*SyncSnapshotCacheBinding, *BuiltSyncSnapshot)
		clock  string
		want   error
	}{
		{name: "invalid generation", mutate: func(binding *SyncSnapshotCacheBinding, _ *BuiltSyncSnapshot) { binding.Generation = 0 }},
		{name: "invalid page size", mutate: func(binding *SyncSnapshotCacheBinding, _ *BuiltSyncSnapshot) { binding.PageSize = 101 }},
		{name: "invalid UUID", mutate: func(binding *SyncSnapshotCacheBinding, _ *BuiltSyncSnapshot) { binding.SnapshotID = uuid.Nil }},
		{name: "underreported array size", mutate: func(_ *SyncSnapshotCacheBinding, data *BuiltSyncSnapshot) { data.Entries.SizeBytes-- }, want: domain.ErrInvalidFileEntryData},
		{name: "invalid JSON item", mutate: func(_ *SyncSnapshotCacheBinding, data *BuiltSyncSnapshot) { data.Entries.Items[0][3] = 0 }, want: domain.ErrInvalidFileEntryData},
		{name: "actual size too large", mutate: func(_ *SyncSnapshotCacheBinding, data *BuiltSyncSnapshot) {
			data.Entries.Items[0] = json.RawMessage(`{"name":"` + strings.Repeat("a", domain.MaxSyncSnapshotBytes) + `"}`)
		}, want: domain.ErrSyncSnapshotTooLarge},
		{name: "invalid clock after transfer", clock: "zero"},
		{name: "canceled after transfer", clock: "cancel", want: context.Canceled},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cache := NewSyncSnapshotCache(func() time.Time {
				if testCase.clock == "cancel" {
					cancel()
				}
				if testCase.clock == "zero" {
					return time.Time{}
				}
				return time.Now()
			})
			binding, data := syncSnapshotCacheFixture()
			if testCase.mutate != nil {
				testCase.mutate(&binding, data)
			}
			reservation, err := cache.ReserveBuild(ctx)
			if err != nil {
				t.Fatal(err)
			}
			expiresAt, err := cache.Put(ctx, reservation, binding, data)
			if err == nil || (testCase.want != nil && !errors.Is(err, testCase.want)) || !expiresAt.IsZero() || len(cache.entries) != 0 {
				t.Fatalf("失败不得保存快照，到期=%s，错误=%v", expiresAt, err)
			}
			assertSnapshotCacheCapacity(t, cache, 0, 0, 0)
			// 2. 失败释放后，新的构建可重新申请额度。
			newBuild, err := cache.ReserveBuild(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			newBuild.Release()
		})
	}
}

func TestSyncSnapshotCachePutConcurrentDuplicate(t *testing.T) {
	// 1. 两次构建竞争同一个标识，只有一次入缓存。
	stamp := time.Date(2026, 11, 23, 2, 0, 0, 0, time.UTC)
	cache := NewSyncSnapshotCache(func() time.Time { return stamp })
	binding, snapshot := syncSnapshotCacheFixture()
	// 使用合法空数组，同时核对缓存对零条目快照的写入。
	snapshot.Entries = domain.SyncSnapshotEntriesJSON{Items: []json.RawMessage{}, SizeBytes: 2}
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		reservation, err := cache.ReserveBuild(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			<-start
			_, err := cache.Put(t.Context(), reservation, binding, snapshot)
			results <- err
		}()
	}
	close(start)
	first, second := <-results, <-results
	if (first == nil) == (second == nil) || len(cache.entries) != 1 {
		t.Fatalf("并发重复写入结果异常，错误=%v、%v", first, second)
	}

	// 2. 失败的一方释放预算，原快照的时刻和唯一槽位保持不变。
	stored := cache.entries[binding.SnapshotID]
	if stored.expiresAt != stamp.Add(10*time.Minute) || stored.snapshot.Entries.Items == nil || len(stored.snapshot.Entries.Items) != 0 {
		t.Fatal("重复写入改变了已保存的到期时刻或空树")
	}
	assertSnapshotCacheCapacity(t, cache, 0, 1, 2)
}
