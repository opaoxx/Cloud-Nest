package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func syncSnapshotReplayDeliveryFixture(t *testing.T) *SyncSnapshotPageDelivery {
	t.Helper()
	key := bytes.Repeat([]byte{0x61}, sha256.Size)
	cursorSigner, err := NewSyncSnapshotCursorSigner(key)
	if err != nil {
		t.Fatal(err)
	}
	baselineSigner, err := NewSyncSnapshotBaselineSigner(key)
	if err != nil {
		t.Fatal(err)
	}
	return NewSyncSnapshotPageDelivery(cursorSigner.Sign, baselineSigner.Sign)
}

func TestSyncSnapshotReplayFirstPageStaysFixed(t *testing.T) {
	for _, itemCount := range []int{0, 1, 2} {
		t.Run([]string{"empty tree", "only page", "with next page"}[itemCount], func(t *testing.T) {
			// 1. 通过真实缓存和签发器保存固定的第一页。
			now := time.Date(2026, 11, 23, 2, 0, 0, 123456789, time.UTC)
			cache := NewSyncSnapshotCache(func() time.Time { return now })
			binding, data := syncSnapshotCacheFixture()
			binding.PageSize = 1
			switch itemCount {
			case 0:
				data.Entries.Items = []json.RawMessage{}
				data.Entries.SizeBytes = 2
			case 2:
				secondItem := strings.Replace(snapshotCacheFixtureItem, `"entry_id":"12"`, `"entry_id":"13"`, 1)
				data.Entries.Items = append(data.Entries.Items, json.RawMessage(secondItem))
				data.Entries.SizeBytes += 1 + len(secondItem)
			}
			expiresAt := putSyncSnapshotForReadTest(t, cache, binding, data)
			delivery := syncSnapshotReplayDeliveryFixture(t)
			firstView, err := cache.Get(t.Context(), binding)
			if err != nil {
				t.Fatal(err)
			}
			firstPage, err := firstView.Page(0)
			if err != nil {
				t.Fatal(err)
			}
			want, err := delivery.Build(t.Context(), binding, data.Header, expiresAt, firstPage)
			if err != nil {
				t.Fatal(err)
			}

			// 2. 在同一原期限内重读，逐项比较首次交付和重读的页面与凭据。
			actor := domain.Actor{UserID: binding.UserID, DeviceID: binding.DeviceID, SessionID: 71}
			locator := domain.SyncSnapshotReceiptLocator{SnapshotID: binding.SnapshotID, DeviceID: binding.DeviceID, Generation: binding.Generation}
			reader := NewSyncSnapshotReplayService(func(_ context.Context, userID, deviceID uint64) (domain.SyncCursorGeneration, error) {
				if userID != actor.UserID || deviceID != actor.DeviceID {
					t.Fatal("代次查询未使用认证身份")
				}
				return binding.Generation, nil
			}, cache.Get, delivery)
			for _, readAt := range []time.Time{now, expiresAt.Add(-time.Nanosecond)} {
				now = readAt
				actual, err := reader.ReadFirstPage(t.Context(), actor, locator, binding.PageSize)
				if err != nil || !reflect.DeepEqual(actual, want) {
					t.Fatalf("重读结果为 %+v，错误 %v，期望 %+v", actual, err, want)
				}
				if actual.Items == nil || len(actual.Items) != min(1, itemCount) || (actual.NextCursor != nil) != (itemCount == 2) ||
					(actual.BaselineToken != nil) != (itemCount < 2) {
					t.Fatalf("第一页条目或凭据不符 %+v", actual)
				}
			}

			// 3. 到达原期限时失效，之前的重读不改变到期时刻。
			now = expiresAt
			actual, err := reader.ReadFirstPage(t.Context(), actor, locator, binding.PageSize)
			if actual != nil || !errors.Is(err, domain.ErrSyncSnapshotExpired) {
				t.Fatalf("到期时重读结果为 %+v，错误 %v", actual, err)
			}
		})
	}
}

func TestSyncSnapshotReplayFirstPageRejectsInvalidOrUnavailableSnapshot(t *testing.T) {
	queryFailure := errors.New("fixture query failed")
	for _, testCase := range []struct {
		scenario            string
		want                error
		wantFind, wantCache int
	}{
		{"receipt device mismatch", domain.ErrInvalidIdempotencyRecordData, 0, 0},
		{"invalid page size", domain.ErrInvalidIdempotencyRecordData, 0, 0},
		{"missing sync state", domain.ErrSyncSnapshotExpired, 1, 0},
		{"changed generation", domain.ErrSyncSnapshotExpired, 1, 0},
		{"revoked device", domain.ErrDeviceRevoked, 1, 0},
		{"query error", queryFailure, 1, 0},
		{"cancellation during query", context.Canceled, 1, 0},
		{"missing cache", domain.ErrSyncSnapshotExpired, 1, 1},
		{"different cached page size", domain.ErrInvalidIdempotencyRecordData, 1, 1},
		{"cancellation during cache read", context.Canceled, 1, 1},
	} {
		t.Run(testCase.scenario, func(t *testing.T) {
			// 1. 构造固定快照，只改变当前场景对应的输入或依赖。
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			now := time.Date(2026, 11, 23, 2, 0, 0, 0, time.UTC)
			cache := NewSyncSnapshotCache(func() time.Time { return now })
			binding, data := syncSnapshotCacheFixture()
			putSyncSnapshotForReadTest(t, cache, binding, data)
			actor := domain.Actor{UserID: binding.UserID, DeviceID: binding.DeviceID, SessionID: 71}
			locator := domain.SyncSnapshotReceiptLocator{SnapshotID: binding.SnapshotID, DeviceID: binding.DeviceID, Generation: binding.Generation}
			pageSize := binding.PageSize
			switch testCase.scenario {
			case "receipt device mismatch":
				locator.DeviceID++
			case "invalid page size":
				pageSize = 0
			case "missing cache":
				locator.SnapshotID[15] ^= 1
			case "different cached page size":
				pageSize++
			}

			// 2. 记录代次查询和缓存读取的准确顺序，失败时不进入交付。
			findCalls, cacheCalls := 0, 0
			reader := NewSyncSnapshotReplayService(func(context.Context, uint64, uint64) (domain.SyncCursorGeneration, error) {
				findCalls++
				generation := binding.Generation
				switch testCase.scenario {
				case "missing sync state":
					return domain.SyncCursorGeneration{}, domain.ErrSyncDeviceNotFound
				case "changed generation":
					generation[15] ^= 1
				case "revoked device":
					return domain.SyncCursorGeneration{}, domain.ErrDeviceRevoked
				case "query error":
					return domain.SyncCursorGeneration{}, queryFailure
				case "cancellation during query":
					cancel()
				}
				return generation, nil
			}, func(requestContext context.Context, key SyncSnapshotCacheBinding) (*CachedSyncSnapshot, error) {
				cacheCalls++
				if findCalls != 1 || key.SnapshotID != locator.SnapshotID || key.UserID != actor.UserID ||
					key.DeviceID != actor.DeviceID || key.Generation != locator.Generation || key.PageSize != pageSize {
					t.Fatal("缓存读取未使用复核后的绑定")
				}
				view, err := cache.Get(requestContext, key)
				if testCase.scenario == "cancellation during cache read" {
					cancel()
				}
				return view, err
			}, syncSnapshotReplayDeliveryFixture(t))
			result, err := reader.ReadFirstPage(ctx, actor, locator, pageSize)
			if result != nil || !errors.Is(err, testCase.want) || findCalls != testCase.wantFind || cacheCalls != testCase.wantCache {
				t.Fatalf("重读结果为 %+v，错误 %v，代次查询=%d，缓存读取=%d", result, err, findCalls, cacheCalls)
			}
		})
	}
}
