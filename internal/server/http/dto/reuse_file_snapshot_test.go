package dto

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const reuseFileSnapshotJSON = `{"entry":{"id":"18446744073709551615","parent_id":"18446744073709551614","name":"合成分片 (1).bin","entry_type":"FILE","size_bytes":9007199254740991,"revision":"1","created_at":"2026-11-21T08:00:00.123456Z","modified_at":"2026-11-21T08:00:00.123456Z","updated_at":"2026-11-21T08:00:00.123456Z","status":"ACTIVE"}}`

func reuseFileSnapshotFixture() domain.FileEntry {
	createdAt := time.Date(2026, 11, 21, 16, 0, 0, 123456789, time.FixedZone("fixture UTC+8", 8*60*60))
	return domain.FileEntry{
		ID: math.MaxUint64, UserID: 7, ParentID: new(uint64(math.MaxUint64 - 1)), Name: "合成分片 (1).bin",
		EntryType: "FILE", FileSize: 1<<53 - 1, Revision: 1, Status: "ACTIVE",
		CreatedAt: createdAt, ModifiedAt: createdAt, UpdatedAt: createdAt,
	}
}

func TestReuseFileSnapshotRoundTrip(t *testing.T) {
	// 1. 编码完整公开字段，保留大 ID、大小、实际名称与 UTC 微秒。
	entry := reuseFileSnapshotFixture()
	data, err := NewReuseFileData(&entry)
	if err != nil || data == nil {
		t.Fatal("构造秒传快照失败", err)
	}
	body, err := json.Marshal(data)
	if err != nil || string(body) != reuseFileSnapshotJSON {
		t.Fatalf("公开快照不符，得到 %s 和 %v", body, err)
	}

	// 2. 模拟数据库 JSON 调序，恢复原结果及独立副本。
	var unordered map[string]json.RawMessage
	if err := json.Unmarshal(body, &unordered); err != nil {
		t.Fatal(err)
	}
	var entryFields map[string]json.RawMessage
	if err := json.Unmarshal(unordered["entry"], &entryFields); err != nil {
		t.Fatal(err)
	}
	reordered, err := json.Marshal(map[string]any{"entry": entryFields})
	if err != nil {
		t.Fatal(err)
	}
	entry.Name, *entry.ParentID, entry.Revision = "已改名文件", 11, 2
	restored, err := RestoreReuseFileData(reordered)
	if err != nil || restored == nil || *restored != *data {
		t.Fatalf("快照未恢复到创建时的事实，得到 %+v 和 %v", restored, err)
	}
	reordered[0], data.Entry.Name = '!', "快照副本被编辑"
	if restored.Entry.Name != "合成分片 (1).bin" || restored.Entry.Revision != "1" {
		t.Fatal("恢复结果受到输入修改影响")
	}

	// 3. 零字节文件也能通过编码与恢复。
	empty := reuseFileSnapshotFixture()
	empty.FileSize = 0
	emptyData, err := NewReuseFileData(&empty)
	if err != nil || emptyData == nil {
		t.Fatal("构造零字节快照失败", err)
	}
	emptyBody, err := json.Marshal(emptyData)
	if err != nil {
		t.Fatal(err)
	}
	emptyRestored, err := RestoreReuseFileData(emptyBody)
	if err != nil || emptyRestored == nil || emptyRestored.Entry.SizeBytes != 0 {
		t.Fatalf("零字节恢复不符，得到 %+v 和 %v", emptyRestored, err)
	}
}

func TestReuseFileSnapshotRejectsInvalidResult(t *testing.T) {
	if data, err := NewReuseFileData(nil); data != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
		t.Fatal("空文件不应生成快照", err)
	}
	for _, scenario := range []struct {
		label  string
		change func(*domain.FileEntry)
	}{
		{"missing user", func(entry *domain.FileEntry) { entry.UserID = 0 }},
		{"self parent", func(entry *domain.FileEntry) { entry.ParentID = &entry.ID }},
		{"empty name", func(entry *domain.FileEntry) { entry.Name = "" }},
		{"invalid UTF-8", func(entry *domain.FileEntry) { entry.Name = "bad\xff.bin" }},
		{"directory", func(entry *domain.FileEntry) { entry.EntryType = "DIR" }},
		{"later revision", func(entry *domain.FileEntry) { entry.Revision = 2 }},
		{"inactive", func(entry *domain.FileEntry) { entry.Status = "TRASHED" }},
		{"unsafe size", func(entry *domain.FileEntry) { entry.FileSize = 1 << 53 }},
		{"invalid UTC year", func(entry *domain.FileEntry) { entry.CreatedAt = time.Time{} }},
		{"different creation times", func(entry *domain.FileEntry) { entry.UpdatedAt = entry.UpdatedAt.Add(time.Microsecond) }},
	} {
		t.Run(scenario.label, func(t *testing.T) {
			entry := reuseFileSnapshotFixture()
			scenario.change(&entry)
			if data, err := NewReuseFileData(&entry); data != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
				t.Fatalf("非法结果生成了快照，得到 %+v 和 %v", data, err)
			}
		})
	}
}

func TestReuseFileSnapshotRejectsInvalidJSON(t *testing.T) {
	const expectedSize = `"size_bytes":9007199254740991`
	for _, scenario := range []struct {
		label, old, replacement string
	}{
		{"missing size", expectedSize + ",", ""},
		{"string size", expectedSize, `"size_bytes":"9007199254740991"`},
		{"null size", expectedSize, `"size_bytes":null`},
		{"negative size", expectedSize, `"size_bytes":-1`},
		{"fraction size", expectedSize, `"size_bytes":1.5`},
		{"exponential size", expectedSize, `"size_bytes":1e3`},
		{"unsafe size", expectedSize, `"size_bytes":9007199254740992`},
		{"extra storage field", expectedSize, expectedSize + `,"object_id":"29"`},
		{"duplicate size", expectedSize, `"size_\u0062ytes":0,` + expectedSize},
		{"unexpected field", `"name":`, `"display":`},
		{"empty name", "合成分片 (1).bin", ""},
		{"invalid UTF-8", "合成分片 (1).bin", "bad\xff.bin"},
		{"same entry and parent ID", "18446744073709551614", "18446744073709551615"},
		{"noncanonical ID", "18446744073709551614", "011"},
		{"wrong kind", `"FILE"`, `"DIR"`},
		{"later revision", `"revision":"1"`, `"revision":"2"`},
		{"inactive", `"ACTIVE"`, `"TRASHED"`},
		{"mismatched creation times", "08:00:00.123456Z", "08:00:00.123457Z"},
		{"bad time", "08:00:00.123456Z", "bad-time"},
		{"stored request ID", `{"entry":`, `{"request_id":"older-request","entry":`},
		{"duplicate entry", `{"entry":`, `{"entry":{},"entry":`},
		{"trailing JSON", "}}", "}}{}"},
	} {
		t.Run(scenario.label, func(t *testing.T) {
			body := strings.Replace(reuseFileSnapshotJSON, scenario.old, scenario.replacement, 1)
			if data, err := RestoreReuseFileData([]byte(body)); data != nil || !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
				t.Fatalf("非法快照被恢复，得到 %+v 和 %v", data, err)
			}
		})
	}
}
