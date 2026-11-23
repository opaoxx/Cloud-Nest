package dto

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func pullSyncChangesResultFixture() *domain.SyncChangePullResult {
	return &domain.SyncChangePullResult{
		HighWaterSequence: math.MaxUint64, NextCursor: "AQ.Ag",
		Events: []domain.SyncChangeEvent{
			{
				Sequence: math.MaxUint64 - 1, EntryID: 12, Revision: 2, EventType: "ENTRY_RENAMED",
				Payload: json.RawMessage(" \n" + `{"entry_id":"12","parent_id":"11","name":"目录","revision":"2"}`),
			},
			{
				Sequence: math.MaxUint64, EntryID: math.MaxUint64, Revision: math.MaxUint64, EventType: domain.FileContentReplacedEventType,
				Payload: json.RawMessage(`{"entry_id":"18446744073709551615","parent_id":"11","name":"笔记.md","revision":"18446744073709551615","size_bytes":0,"sha256":"` +
					strings.Repeat("ab", 32) + `"}`),
			},
		},
	}
}

func TestNewPullSyncChangesDataJSON(t *testing.T) {
	// 1. 核对整段 JSON 的字段名和类型，覆盖混合页、续页及两种空页。
	const requestID = "eb9c7469-d6e4-4f95-991c-a8341bcd8755"
	const metadata = `{"sequence":"18446744073709551614","entry_id":"12","revision":"2","event_type":"ENTRY_RENAMED","payload":{"entry_id":"12","parent_id":"11","name":"目录","revision":"2"}}`
	content := `{"sequence":"18446744073709551615","entry_id":"18446744073709551615","revision":"18446744073709551615","event_type":"FILE_CONTENT_REPLACED","payload":{"entry_id":"18446744073709551615","parent_id":"11","name":"笔记.md","revision":"18446744073709551615","size_bytes":0,"sha256":"` +
		strings.Repeat("ab", 32) + `"}}`
	for _, scenario := range []struct {
		name              string
		events, highWater string
		hasMore           string
		change            func(*domain.SyncChangePullResult)
	}{
		{name: "final mixed page", events: "[" + metadata + "," + content + "]", highWater: "18446744073709551615", hasMore: "false"},
		{name: "continuation", events: "[" + metadata + "]", highWater: "18446744073709551615", hasMore: "true",
			change: func(result *domain.SyncChangePullResult) {
				result.Events = result.Events[:1]
				result.HasMore = true
			}},
		{name: "empty at zero", events: "[]", highWater: "0", hasMore: "false",
			change: func(result *domain.SyncChangePullResult) { result.Events, result.HighWaterSequence = nil, 0 }},
		{name: "empty at nonzero upper bound", events: "[]", highWater: "18446744073709551615", hasMore: "false",
			change: func(result *domain.SyncChangePullResult) { result.Events = nil }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			result := pullSyncChangesResultFixture()
			if scenario.change != nil {
				scenario.change(result)
			}
			data, err := NewPullSyncChangesData(result)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(PullSyncChangesResponse{Data: *data, RequestID: requestID})
			want := `{"data":{"events":` + scenario.events + `,"high_water_sequence":"` + scenario.highWater +
				`","next_cursor":"AQ.Ag","has_more":` + scenario.hasMore + `},"request_id":"` + requestID + `"}`
			if err != nil || string(encoded) != want {
				t.Fatalf("公开 JSON 不符：%s，错误 %v，预期 %s", encoded, err, want)
			}
		})
	}
}

func TestNewPullSyncChangesDataRejectsInvalidResult(t *testing.T) {
	// 1. 所有失败均返回空 DTO，避免把部分内容交给 HTTP 层。
	if data, err := NewPullSyncChangesData(nil); data != nil || !errors.Is(err, domain.ErrInvalidSyncChangePage) {
		t.Fatalf("空结果应失败，实际 %+v，错误 %v", data, err)
	}
	for _, scenario := range []struct {
		name   string
		change func(*domain.SyncChangePullResult)
	}{
		{"missing cursor", func(result *domain.SyncChangePullResult) { result.NextCursor = "" }},
		{"malformed cursor", func(result *domain.SyncChangePullResult) { result.NextCursor = "AQ.Ag.Aw" }},
		{"overlong cursor", func(result *domain.SyncChangePullResult) { result.NextCursor = strings.Repeat("a", 1023) + ".b" }},
		{"too many events", func(result *domain.SyncChangePullResult) { result.Events = make([]domain.SyncChangeEvent, 101) }},
		{"zero sequence", func(result *domain.SyncChangePullResult) { result.Events[0].Sequence = 0 }},
		{"zero entry ID", func(result *domain.SyncChangePullResult) { result.Events[0].EntryID = 0 }},
		{"zero revision", func(result *domain.SyncChangePullResult) { result.Events[0].Revision = 0 }},
		{"missing event type", func(result *domain.SyncChangePullResult) { result.Events[0].EventType = "" }},
		{"above upper bound", func(result *domain.SyncChangePullResult) { result.HighWaterSequence-- }},
		{"gap", func(result *domain.SyncChangePullResult) { result.Events[0].Sequence-- }},
		{"out of order", func(result *domain.SyncChangePullResult) {
			result.Events[0], result.Events[1] = result.Events[1], result.Events[0]
		}},
		{"invalid later payload", func(result *domain.SyncChangePullResult) { result.Events[1].Payload = json.RawMessage(`{"entry_id":}`) }},
		{"non-object payload", func(result *domain.SyncChangePullResult) {
			result.Events[0].Payload = json.RawMessage(`["object expected"]`)
		}},
		{"invalid payload text", func(result *domain.SyncChangePullResult) { result.Events[0].Payload = []byte("{\"name\":\"\xff\"}") }},
		{"false after a partial page", func(result *domain.SyncChangePullResult) { result.Events = result.Events[:1] }},
		{"true at upper bound", func(result *domain.SyncChangePullResult) { result.HasMore = true }},
		{"true on an empty page", func(result *domain.SyncChangePullResult) { result.Events, result.HasMore = nil, true }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			result := pullSyncChangesResultFixture()
			scenario.change(result)
			if data, err := NewPullSyncChangesData(result); data != nil || !errors.Is(err, domain.ErrInvalidSyncChangePage) {
				t.Fatalf("异常结果应失败，实际 %+v，错误 %v", data, err)
			}
		})
	}
}

func TestNewPullSyncChangesDataCopiesPayload(t *testing.T) {
	// 1. 修改服务结果的载荷后，已构造的 DTO 的字节仍保持原值。
	result := pullSyncChangesResultFixture()
	data, err := NewPullSyncChangesData(result)
	if err != nil {
		t.Fatal(err)
	}
	before := string(data.Events[0].Payload)
	result.Events[0].Payload[2] = '['
	if string(data.Events[0].Payload) != before {
		t.Fatal("DTO 与服务结果共享载荷字节")
	}
}
