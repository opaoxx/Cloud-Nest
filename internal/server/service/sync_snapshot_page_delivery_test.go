package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestSyncSnapshotPageDeliveryBuild(t *testing.T) {
	// 1. 装配真实签发器和独立校验器，读取中间页、末页和空树。
	master := bytes.Repeat([]byte{0x62}, sha256.Size)
	cursorVerifier, err := NewSyncSnapshotCursorVerifier(master)
	if err != nil {
		t.Fatal(err)
	}
	baselineVerifier, err := NewSyncSnapshotBaselineVerifier(master)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct {
		name                        string
		count, position, next, size int
		sequence                    uint64
	}{
		{name: "first", count: 5, next: 2, size: 2, sequence: 20},
		{name: "middle", count: 5, position: 2, next: 4, size: 2, sequence: 20},
		{name: "last", count: 5, position: 4, size: 1, sequence: math.MaxUint64},
		{name: "empty tree", count: 0, size: 0, sequence: 0},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			snapshot, _ := syncSnapshotForPageTest(t, testCase.count, 2)
			page, err := snapshot.Page(testCase.position)
			if err != nil {
				t.Fatal(err)
			}
			binding, _ := syncSnapshotCacheFixture()
			binding.PageSize = 2
			header := snapshot.Header
			header.HighWaterSequence = testCase.sequence
			cursorCalls, baselineCalls := 0, 0
			delivery := NewSyncSnapshotPageDelivery(func(key SyncSnapshotCacheBinding, position int, expiry time.Time) (string, error) {
				cursorCalls++
				return cursorVerifier.signer.Sign(key, position, expiry)
			}, func(key SyncSnapshotCacheBinding, sequence uint64, expiry time.Time) (string, error) {
				baselineCalls++
				return baselineVerifier.signer.Sign(key, sequence, expiry)
			})
			stamp := snapshot.ExpiresAt.Add(-time.Nanosecond)
			result, err := delivery.Build(t.Context(), binding, header, snapshot.ExpiresAt, page)
			if err != nil || result == nil || result.SnapshotID != binding.SnapshotID || result.Header != header ||
				result.ExpiresAt != snapshot.ExpiresAt || result.Items == nil || len(result.Items) != testCase.size {
				t.Fatalf("快照页数据不符，结果=%+v，错误=%v", result, err)
			}

			// 2. 只调用预期签发器，校验实际凭据并检查原快照数据。
			if testCase.next != 0 {
				if result.NextCursor == nil || result.BaselineToken != nil || cursorCalls != 1 || baselineCalls != 0 {
					t.Fatal("非末页应只签发续页凭据")
				}
				verified, err := cursorVerifier.Verify(binding.UserID, binding.DeviceID, binding.SnapshotID, *result.NextCursor, stamp)
				if err != nil || verified == nil || verified.Binding != binding || verified.NextPosition != testCase.next || verified.ExpiresAt != result.ExpiresAt {
					t.Fatalf("续页凭据绑定、位置或期限不符，错误=%v", err)
				}
			} else {
				if result.NextCursor != nil || result.BaselineToken == nil || cursorCalls != 0 || baselineCalls != 1 {
					t.Fatal("末页或空树应只签发确认凭据")
				}
				verified, err := baselineVerifier.Verify(binding.UserID, binding.DeviceID, binding.SnapshotID, *result.BaselineToken, stamp)
				if err != nil || verified == nil || verified.Generation != binding.Generation || verified.HighWaterSequence != testCase.sequence || verified.ExpiresAt != result.ExpiresAt {
					t.Fatalf("确认凭据代次、序号或期限不符，错误=%v", err)
				}
			}
			for index, item := range page.Items {
				if !bytes.Equal(item, result.Items[index]) {
					t.Fatal("交付没有保留原条目字节和顺序")
				}
				item[0] = '['
				if result.Items[index][0] != '{' {
					t.Fatal("交付结果共享了调用方持有的可变字节")
				}
			}
		})
	}
}

func TestSyncSnapshotPageDeliveryRejectsIncompleteResult(t *testing.T) {
	// 1. 使用故障签发器，检查失败、取消或异常页不会产生部分交付。
	fixtureError := errors.New("fixture signing failed")
	for _, testCase := range []struct {
		name       string
		choice     string
		wantError  error
		wantCursor int
		wantBase   int
	}{
		{"short nonterminal page", "short", domain.ErrInvalidFileEntryData, 0, 0},
		{"invalid item", "invalid", domain.ErrInvalidFileEntryData, 0, 0},
		{"cursor signer failure", "fail", fixtureError, 1, 0},
		{"empty baseline", "empty", nil, 0, 1},
		{"oversized baseline", "large", nil, 0, 1},
		{"canceled while signing", "cancel", context.Canceled, 0, 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			binding, data := syncSnapshotCacheFixture()
			binding.PageSize = 1
			page := &SyncSnapshotPage{Items: []json.RawMessage{json.RawMessage(snapshotCacheFixtureItem)}}
			position := 1
			switch testCase.choice {
			case "short":
				page.Items, page.NextPosition = nil, &position
			case "invalid":
				page.Items[0] = json.RawMessage("null")
			case "fail":
				page.NextPosition = &position
			}
			cursorCalls, baselineCalls := 0, 0
			delivery := NewSyncSnapshotPageDelivery(func(SyncSnapshotCacheBinding, int, time.Time) (string, error) {
				cursorCalls++
				return "fixture", fixtureError
			}, func(SyncSnapshotCacheBinding, uint64, time.Time) (string, error) {
				baselineCalls++
				if testCase.choice == "cancel" {
					cancel()
					return "fixture", nil
				}
				if testCase.choice == "large" {
					return strings.Repeat("a", domain.MaxSyncSnapshotTokenLength+1), nil
				}
				return "", nil
			})
			expiry := time.Date(2026, 11, 23, 2, 10, 0, 0, time.UTC)
			result, err := delivery.Build(ctx, binding, data.Header, expiry, page)
			if result != nil || err == nil || (testCase.wantError != nil && !errors.Is(err, testCase.wantError)) ||
				cursorCalls != testCase.wantCursor || baselineCalls != testCase.wantBase {
				t.Fatalf("错误页结果不符，续页签发=%d，确认签发=%d，错误=%v", cursorCalls, baselineCalls, err)
			}
		})
	}
}
