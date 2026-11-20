package dto

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const savedPurgeEntryAcceptedSample = `{"entry_id":"23","status":"PURGING","progress_url":"/api/v1/trash/23/purge"}`

func TestDecodePurgeEntryAcceptedDataRoundTrip(t *testing.T) {
	for _, entryID := range []uint64{1, 23, math.MaxUint64} {
		entry := purgeEntryAcceptedFixture()
		entry.ID = entryID
		want, err := NewPurgeEntryAcceptedData(entry)
		if err != nil {
			t.Fatal(err)
		}
		t.Run(want.EntryID, func(t *testing.T) {
			// 1. 模拟数据库重排字段并加入格式空白。
			body, err := json.MarshalIndent(map[string]string{
				"progress_url": want.ProgressURL, "entry_id": want.EntryID, "status": want.Status,
			}, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			first, err := DecodePurgeEntryAcceptedData(body)
			if err != nil || first == nil || *first != *want {
				t.Fatal("回读受理快照内容不符", first, err)
			}
			second, err := DecodePurgeEntryAcceptedData(body)
			if err != nil || second == nil {
				t.Fatal("第二次回读失败", err)
			}

			// 2. 输出与输入字节、另一份解码结果各自独立。
			clear(body)
			first.ProgressURL = "/later-change"
			if *second != *want {
				t.Fatal("回读结果随输入字节或另一份结果改变", second)
			}
		})
	}
}

func TestDecodePurgeEntryAcceptedDataRejectsInvalidSnapshot(t *testing.T) {
	for _, scenario := range []struct {
		name, body string
	}{
		{"empty", ""},
		{"null", "null"},
		{"array", "[]"},
		{"unclosed", strings.TrimSuffix(savedPurgeEntryAcceptedSample, "}")},
		{"trailing JSON", savedPurgeEntryAcceptedSample + " {}"},
		{"invalid UTF-8", strings.Replace(savedPurgeEntryAcceptedSample, "PURGING", "PURG\xffNG", 1)},
		{"response wrapper", `{"data":` + savedPurgeEntryAcceptedSample + `,"request_id":"fixture"}`},
		{"unknown field", strings.TrimSuffix(savedPurgeEntryAcceptedSample, "}") + `,"request_id":"fixture"}`},
		{"missing field", `{"entry_id":"23","status":"PURGING"}`},
		{"duplicate key", `{"entry_id":"23",` + savedPurgeEntryAcceptedSample[1:]},
		{"escaped duplicate key", `{"entry\u005fid":"23",` + savedPurgeEntryAcceptedSample[1:]},
		{"wrong case key", strings.Replace(savedPurgeEntryAcceptedSample, "entry_id", "Entry_ID", 1)},
		{"numeric ID", strings.Replace(savedPurgeEntryAcceptedSample, `"23"`, "23", 1)},
		{"zero ID", strings.ReplaceAll(savedPurgeEntryAcceptedSample, "23", "0")},
		{"leading zero ID", strings.ReplaceAll(savedPurgeEntryAcceptedSample, "23", "023")},
		{"overflow ID", strings.ReplaceAll(savedPurgeEntryAcceptedSample, "23", "18446744073709551616")},
		{"null status", strings.Replace(savedPurgeEntryAcceptedSample, `"PURGING"`, "null", 1)},
		{"finished state", strings.Replace(savedPurgeEntryAcceptedSample, "PURGING", "PURGED", 1)},
		{"path for another target", strings.Replace(savedPurgeEntryAcceptedSample, "/23/purge", "/24/purge", 1)},
		{"absolute URL", strings.Replace(savedPurgeEntryAcceptedSample, "/api/", "https://example.test/api/", 1)},
		{"path with query", strings.Replace(savedPurgeEntryAcceptedSample, "/23/purge", "/23/purge?extra=1", 1)},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			if result, err := DecodePurgeEntryAcceptedData([]byte(scenario.body)); result != nil || !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
				t.Fatal("异常受理快照应返回固定的幂等记录错误", result, err)
			}
		})
	}
}
