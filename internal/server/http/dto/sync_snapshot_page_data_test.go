package dto

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func syncSnapshotPageDataFixture(t *testing.T) *domain.SyncSnapshotPageResult {
	t.Helper()
	encoded, err := EncodeSyncSnapshotEntries([]domain.SyncSnapshotEntry{
		{ID: 12, ParentID: math.MaxUint64, Name: "目录&", EntryType: "DIR", Revision: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &domain.SyncSnapshotPageResult{
		SnapshotID: uuid.MustParse("052dc4f7-f4c1-46b3-a509-bf96992f6580"),
		Header:     domain.SyncSnapshotHeader{RootEntryID: math.MaxUint64, HighWaterSequence: math.MaxUint64},
		ExpiresAt:  time.Date(2026, 11, 23, 10, 10, 0, 123456789, time.FixedZone("UTC+8", 8*60*60)),
		Items:      encoded.Items, NextCursor: new("AQ.Ag"),
	}
}

func TestNewSyncSnapshotPageDataJSON(t *testing.T) {
	// 1. 对照真实 JSON 字段和类型，分别验证续页、末页和空树。
	const item = `[{"entry_id":"12","parent_id":"18446744073709551615","name":"目录\u0026","entry_type":"DIR","revision":"2","size_bytes":0,"sha256":null}]`
	for _, testCase := range []struct {
		name                 string
		sequence, items      string
		nextCursor, baseline string
		change               func(*domain.SyncSnapshotPageResult)
	}{
		{name: "continuation", sequence: `"18446744073709551615"`, items: item, nextCursor: `"AQ.Ag"`, baseline: "null"},
		{name: "last page", sequence: `"18446744073709551615"`, items: item, nextCursor: "null", baseline: `"Aw.BA"`, change: func(result *domain.SyncSnapshotPageResult) {
			result.NextCursor, result.BaselineToken = nil, new("Aw.BA")
		}},
		{name: "empty tree", sequence: `"0"`, items: "[]", nextCursor: "null", baseline: `"Aw.BA"`, change: func(result *domain.SyncSnapshotPageResult) {
			result.NextCursor, result.BaselineToken = nil, new("Aw.BA")
			result.Header.HighWaterSequence, result.Items = 0, nil
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			result := syncSnapshotPageDataFixture(t)
			if testCase.change != nil {
				testCase.change(result)
			}
			data, err := NewSyncSnapshotPageData(result)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			var actual map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &actual); err != nil {
				t.Fatal(err)
			}
			want := map[string]json.RawMessage{
				"snapshot_id":         json.RawMessage(`"052dc4f7-f4c1-46b3-a509-bf96992f6580"`),
				"root_entry_id":       json.RawMessage(`"18446744073709551615"`),
				"high_water_sequence": json.RawMessage(testCase.sequence),
				"expires_at":          json.RawMessage(`"2026-11-23T02:10:00.123456789Z"`),
				"items":               json.RawMessage(testCase.items),
				"next_cursor":         json.RawMessage(testCase.nextCursor),
				"baseline_token":      json.RawMessage(testCase.baseline),
			}
			if !reflect.DeepEqual(actual, want) {
				t.Fatalf("公开 JSON 不符合快照页面契约，实际=%s", encoded)
			}
		})
	}
}

func TestNewSyncSnapshotPageDataRejectsInvalidResult(t *testing.T) {
	// 1. 拒绝损坏的页头、条目和不互斥的凭据。
	if data, err := NewSyncSnapshotPageData(nil); data != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
		t.Fatal("应拒绝空页面结果")
	}
	for _, testCase := range []struct {
		name   string
		change func(*domain.SyncSnapshotPageResult)
	}{
		{"wrong snapshot UUID", func(result *domain.SyncSnapshotPageResult) { result.SnapshotID[6] = 0x10 }},
		{"wrong snapshot UUID variant", func(result *domain.SyncSnapshotPageResult) { result.SnapshotID[8] = 0 }},
		{"zero root", func(result *domain.SyncSnapshotPageResult) { result.Header.RootEntryID = 0 }},
		{"zero expiry", func(result *domain.SyncSnapshotPageResult) { result.ExpiresAt = time.Time{} }},
		{"UTC expiry out of range", func(result *domain.SyncSnapshotPageResult) {
			result.ExpiresAt = time.Date(9999, 12, 31, 23, 30, 0, 0, time.FixedZone("UTC-1", -3600))
		}},
		{"both tokens", func(result *domain.SyncSnapshotPageResult) { result.BaselineToken = new("Aw.BA") }},
		{"neither token", func(result *domain.SyncSnapshotPageResult) { result.NextCursor = nil }},
		{"empty continuation", func(result *domain.SyncSnapshotPageResult) { result.Items = nil }},
		{"blank token", func(result *domain.SyncSnapshotPageResult) { *result.NextCursor = "" }},
		{"extra token segment", func(result *domain.SyncSnapshotPageResult) { *result.NextCursor = "AQ.Ag.Aw" }},
		{"invalid baseline format", func(result *domain.SyncSnapshotPageResult) {
			result.NextCursor, result.BaselineToken = nil, new("Aw.BA\n")
		}},
		{"overlong token", func(result *domain.SyncSnapshotPageResult) { *result.NextCursor = strings.Repeat("a", 1023) + ".b" }},
		{"too many items", func(result *domain.SyncSnapshotPageResult) { result.Items = make([]json.RawMessage, 101) }},
		{"invalid item JSON", func(result *domain.SyncSnapshotPageResult) { result.Items[0] = json.RawMessage(`{"id":}`) }},
		{"item is not an object", func(result *domain.SyncSnapshotPageResult) { result.Items[0] = json.RawMessage(`null`) }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			input := syncSnapshotPageDataFixture(t)
			testCase.change(input)
			if data, err := NewSyncSnapshotPageData(input); data != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
				t.Fatalf("异常页面应返回数据错误，错误=%v", err)
			}
		})
	}
}

func TestNewSyncSnapshotPageDataCopiesResult(t *testing.T) {
	// 1. 转换后改动原始 JSON 和凭据，公开页面仍保持原值。
	result := syncSnapshotPageDataFixture(t)
	data, err := NewSyncSnapshotPageData(result)
	if err != nil {
		t.Fatal(err)
	}
	result.Items[0][0] = '['
	*result.NextCursor = "Aw.BA"
	if data.Items[0][0] != '{' || *data.NextCursor != "AQ.Ag" {
		t.Fatal("公开页面与输入共享了可变值")
	}
}
