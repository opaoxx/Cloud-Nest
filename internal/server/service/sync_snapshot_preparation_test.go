package service

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

func TestSyncSnapshotPreparationFixesBuiltSnapshot(t *testing.T) {
	// 1. 用真实构建器和编码器，检查读取事务开启前已经预留预算。
	const wantItem = `{"entry_id":"12","parent_id":"11","name":"目录","entry_type":"DIR","revision":"3","size_bytes":0,"sha256":null}`
	stamp := time.Date(2026, 11, 23, 2, 0, 0, 123456789, time.UTC)
	cache := NewSyncSnapshotCache(func() time.Time { return stamp })
	actor := domain.Actor{UserID: 7, DeviceID: 19, SessionID: 71}
	header := domain.SyncSnapshotHeader{RootEntryID: 11, HighWaterSequence: math.MaxUint64}
	readTransactionOpen := false
	builder := NewSyncSnapshotBuilder(
		func(ctx context.Context, userID uint64, action func(context.Context, SyncSnapshotReader, domain.SyncSnapshotHeader) error) error {
			if userID != actor.UserID {
				t.Fatal("快照构建未使用认证账号")
			}
			cachedCount := len(cache.entries)
			assertSnapshotCacheCapacity(t, cache, 1, 1+cachedCount, domain.MaxSyncSnapshotBytes+cachedCount*(len(wantItem)+2))
			readTransactionOpen = true
			defer func() { readTransactionOpen = false }()
			return action(ctx, syncSnapshotReadFunc(func(context.Context) ([]domain.SyncSnapshotEntry, error) {
				return []domain.SyncSnapshotEntry{{ID: 12, ParentID: 11, Name: "目录", EntryType: "DIR", Revision: 3}}, nil
			}), header)
		}, func(entries []domain.SyncSnapshotEntry) (*domain.SyncSnapshotEntriesJSON, error) {
			if readTransactionOpen {
				t.Fatal("仍在只读事务内时开始编码")
			}
			return dto.EncodeSyncSnapshotEntries(entries)
		})
	preparation := NewSyncSnapshotPreparation(builder.Build, cache)

	// 2. 核对两次结果的固定元数据、条目和独立代次。
	var previous *PreparedSyncSnapshot
	for _, pageSize := range []int{1, 100} {
		prepared, err := preparation.Prepare(t.Context(), actor, pageSize)
		if err != nil || prepared == nil {
			t.Fatal("快照准备失败", err)
		}
		binding := prepared.Binding
		if !binding.valid() || binding.UserID != actor.UserID || binding.DeviceID != actor.DeviceID || binding.PageSize != pageSize ||
			prepared.Header != header || !prepared.ExpiresAt.Equal(stamp.Add(syncSnapshotCacheTTL)) {
			t.Fatalf("准备结果绑定或快照头部错误：%+v", prepared)
		}
		view, err := cache.Get(t.Context(), binding)
		if err != nil || view == nil || view.Header != header || view.ExpiresAt != prepared.ExpiresAt {
			t.Fatalf("缓存中的固定视图错误：%+v，%v", view, err)
		}
		if item, ok := view.Item(0); !ok || string(item) != wantItem || view.ItemCount() != 1 {
			t.Fatalf("缓存条目为 %s，期望 %s", item, wantItem)
		}
		if previous != nil && (previous.Binding.SnapshotID == binding.SnapshotID || previous.Binding.Generation == binding.Generation) {
			t.Fatal("独立准备复用了快照 ID 或设备代次")
		}
		t.Cleanup(func() {
			if !cache.DiscardUncommitted(binding, prepared.reservation) {
				t.Error("准备结果的凭据无法撤回本次缓存")
			}
		})
		previous = prepared
	}
	assertSnapshotCacheCapacity(t, cache, 0, 2, 2*(len(wantItem)+2))
}

func TestSyncSnapshotPreparationFailureReleasesBudget(t *testing.T) {
	readFailure := errors.New("fixture snapshot read failed")
	for _, testCase := range []struct {
		name string
		want error
	}{
		{"builder error", readFailure},
		{"oversized snapshot", domain.ErrSyncSnapshotTooLarge},
		{"invalid snapshot for cache", domain.ErrInvalidFileEntryData},
		{"canceled during build", context.Canceled},
		{"canceled during cache insert", context.Canceled},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cache := NewSyncSnapshotCache(func() time.Time {
				if testCase.name == "canceled during cache insert" {
					cancel()
				}
				return time.Date(2026, 11, 23, 2, 0, 0, 0, time.UTC)
			})
			_, data := syncSnapshotCacheFixture()
			preparation := NewSyncSnapshotPreparation(func(context.Context, domain.Actor) (*BuiltSyncSnapshot, error) {
				switch testCase.name {
				case "builder error":
					return nil, readFailure
				case "oversized snapshot":
					return nil, domain.ErrSyncSnapshotTooLarge
				case "invalid snapshot for cache":
					data.Entries.SizeBytes++
				case "canceled during build":
					cancel()
				}
				return data, nil
			}, cache)
			actor := domain.Actor{UserID: 7, DeviceID: 19, SessionID: 71}
			prepared, err := preparation.Prepare(ctx, actor, 50)
			if prepared != nil || !errors.Is(err, testCase.want) || len(cache.entries) != 0 {
				t.Fatalf("失败结果为 %+v，错误 %v，期望 %v", prepared, err, testCase.want)
			}
			assertSnapshotCacheCapacity(t, cache, 0, 0, 0)
		})
	}
}

func TestSyncSnapshotPreparationRejectsBeforeBuilding(t *testing.T) {
	cache := NewSyncSnapshotCache(time.Now)
	preparation := NewSyncSnapshotPreparation(func(context.Context, domain.Actor) (*BuiltSyncSnapshot, error) {
		t.Fatal("拒绝输入或准入失败后仍访问构建器")
		return nil, nil
	}, cache)
	actor := domain.Actor{UserID: 7, DeviceID: 19, SessionID: 71}
	if result, err := preparation.Prepare(t.Context(), actor, 101); result != nil || err == nil {
		t.Fatal("非法页大小通过了准备")
	}
	if result, err := preparation.Prepare(t.Context(), domain.Actor{}, 50); result != nil || err == nil {
		t.Fatal("缺少身份的调用通过了准备")
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if result, err := preparation.Prepare(canceled, actor, 50); result != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("取消的请求通过了准备", err)
	}
	for range maxConcurrentSyncSnapshotBuilds {
		reservation, err := cache.ReserveBuild(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer reservation.Release()
	}
	if result, err := preparation.Prepare(t.Context(), actor, 50); result != nil || !errors.Is(err, domain.ErrSyncSnapshotBusy) {
		t.Fatal("构建名额已满的请求通过了准备", err)
	}
	assertSnapshotCacheCapacity(t, cache, 2, 2, 2*domain.MaxSyncSnapshotBytes)
}
