package dto

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const savedRestoreEntrySample = `{"entry":{"id":"23","parent_id":"11","name":"学习笔记 (1).md","entry_type":"FILE","status":"ACTIVE","revision":"5"}}`

func TestDecodeRestoreEntryDataRoundTrip(t *testing.T) {
	for _, entryType := range []string{"FILE", "DIR"} {
		t.Run(entryType, func(t *testing.T) {
			result := restoredEntryResponseFixture()
			if entryType == "DIR" {
				result.EntryType, result.FileSize, result.Name = "DIR", 0, "Cafe\u0301.V2 (1) 🌱"
				result.ID, *result.ParentID, result.Revision = math.MaxUint64, math.MaxUint64-1, math.MaxUint64
			}
			want, err := NewRestoreEntryData(result)
			if err != nil {
				t.Fatal(err)
			}

			// 1. 通过另一个 JSON 编码顺序模拟数据库重新排列字段。
			var object map[string]map[string]string
			if err := json.Unmarshal([]byte(stringMustJSON(t, want)), &object); err != nil {
				t.Fatal(err)
			}
			body, err := json.MarshalIndent(object, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			before := bytes.Clone(body)
			first, err := DecodeRestoreEntryData(body)
			if err != nil || first == nil || *first != *want || !bytes.Equal(body, before) {
				t.Fatal("回读内容或输入副本错误", first, err)
			}

			// 2. 同一快照的两次结果独立于原正文，也彼此独立。
			second, err := DecodeRestoreEntryData(body)
			if err != nil || second == nil {
				t.Fatal(err)
			}
			clear(body)
			first.Entry.Name = "later change"
			if *second != *want {
				t.Fatal("回读结果被原字节或其他结果改动", second)
			}
		})
	}
}

func TestDecodeRestoreEntryDataRejectsInvalidShape(t *testing.T) {
	for _, scenario := range []struct {
		name, body string
	}{
		{"empty", ""},
		{"null body", "null"},
		{"array body", "[]"},
		{"missing entry", "{}"},
		{"null entry", `{"entry":null}`},
		{"array entry", `{"entry":[]}`},
		{"wrong case entry", strings.Replace(savedRestoreEntrySample, `"entry"`, `"Entry"`, 1)},
		{"duplicate entry", strings.Replace(savedRestoreEntrySample, `{"entry":`, `{"entry":{},"entry":`, 1)},
		{"outer request ID", strings.TrimSuffix(savedRestoreEntrySample, "}") + `,"request_id":"fixture"}`},
		{"HTTP response wrapper", `{"data":` + savedRestoreEntrySample + `,"request_id":"fixture"}`},
		{"duplicate id", strings.Replace(savedRestoreEntrySample, `"id":"23"`, `"id":"23","id":"23"`, 1)},
		{"escaped duplicate id", strings.Replace(savedRestoreEntrySample, `"id":"23"`, `"id":"23","i\u0064":"23"`, 1)},
		{"extra deleted time", strings.Replace(savedRestoreEntrySample, `"id":"23"`, `"id":"23","deleted_at":"2026-11-20T00:00:00Z"`, 1)},
		{"trailing JSON", savedRestoreEntrySample + " {}"},
		{"unclosed object", strings.TrimSuffix(savedRestoreEntrySample, "}")},
		{"invalid UTF-8", strings.Replace(savedRestoreEntrySample, "学习笔记", "\xff", 1)},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			if result, err := DecodeRestoreEntryData([]byte(scenario.body)); result != nil || !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
				t.Fatal("异常快照结构被接受", result, err)
			}
		})
	}

	for _, field := range []string{"id", "parent_id", "name", "entry_type", "status", "revision"} {
		for _, variant := range []string{"missing", "null", "number", "unknown in place of required"} {
			t.Run(field+"/"+variant, func(t *testing.T) {
				var object map[string]map[string]json.RawMessage
				if err := json.Unmarshal([]byte(savedRestoreEntrySample), &object); err != nil {
					t.Fatal(err)
				}
				entry := object["entry"]
				switch variant {
				case "missing":
					delete(entry, field)
				case "null":
					entry[field] = json.RawMessage("null")
				case "number":
					entry[field] = json.RawMessage("1")
				case "unknown in place of required":
					entry[strings.ToUpper(field)] = entry[field]
					delete(entry, field)
				}
				if result, err := DecodeRestoreEntryData([]byte(stringMustJSON(t, object))); result != nil || !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
					t.Fatal("必填字符串字段结构未校验", result, err)
				}
			})
		}
	}
}

func TestDecodeRestoreEntryDataRejectsInvalidValues(t *testing.T) {
	for _, scenario := range []struct {
		field, value string
	}{
		{"id", "0"}, {"id", "023"}, {"id", "18446744073709551616"},
		{"parent_id", "0"}, {"parent_id", "23"}, {"parent_id", "-11"},
		{"revision", "0"}, {"revision", "+5"}, {"revision", " 5"}, {"revision", "18446744073709551616"},
		{"name", ""}, {"entry_type", "OTHER"}, {"status", "TRASHED"}, {"status", "PURGING"}, {"status", "PURGED"},
	} {
		t.Run(scenario.field+"/"+scenario.value, func(t *testing.T) {
			var object map[string]map[string]string
			if err := json.Unmarshal([]byte(savedRestoreEntrySample), &object); err != nil {
				t.Fatal(err)
			}
			object["entry"][scenario.field] = scenario.value
			if result, err := DecodeRestoreEntryData([]byte(stringMustJSON(t, object))); result != nil || !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
				t.Fatal("不符合恢复契约的字段值被接受", result, err)
			}
		})
	}
}
