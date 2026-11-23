package domain

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
)

func TestValidateSyncChangeContentEvent(t *testing.T) {
	// 1. 分别用现有发布和替换写入方法生成合成事件。
	parentID := uint64(13)
	entry := FileEntry{ID: 31, ParentID: &parentID, Name: "学习笔记 📝.bin", EntryType: "FILE", Status: "ACTIVE", Revision: 1}
	published, err := NewFilePublishedChangeEvent(&entry, sha256.Sum256(nil))
	if err != nil {
		t.Fatal(err)
	}
	entry.ID, entry.Revision, entry.FileSize = math.MaxUint64, math.MaxUint64, 1<<53-1
	replaced, err := NewFileContentReplacedChangeEvent(&entry, UploadContentDigest{
		SizeBytes: entry.FileSize, SHA256: sha256.Sum256([]byte("synthetic digest fixture")),
	})
	if err != nil {
		t.Fatal(err)
	}

	// 2. 核对零字节、最大安全整数和完整 uint64 标识都能读回。
	for _, written := range []ChangeEventInput{published, replaced} {
		t.Run(written.EventType, func(t *testing.T) {
			event := SyncChangeEvent{
				Sequence: 3, EntryID: written.EntryID, Revision: written.Revision, EventType: written.EventType, Payload: written.Payload,
			}
			before := string(event.Payload)
			if err := ValidateSyncChangeContentEvent(event); err != nil {
				t.Fatalf("写入端生成的事件被拒绝：%v", err)
			}
			if string(event.Payload) != before {
				t.Fatal("校验改写了事件载荷")
			}
		})
	}
}

func TestValidateSyncChangeContentEventRejectsMalformedData(t *testing.T) {
	// 1. 逐项修改六字段载荷，覆盖容易与有效内容混淆的缺失、类型和范围。
	digest := strings.Repeat("ab", sha256.Size)
	valid := fmt.Sprintf(`{"entry_id":"31","parent_id":"13","name":"note.bin","revision":"2","size_bytes":0,"sha256":"%s"}`, digest)
	for _, scenario := range []struct {
		name    string
		payload string
	}{
		{"null payload", "null"},
		{"trailing object", valid + " {}"},
		{"missing size", strings.Replace(valid, `"size_bytes":0,`, "", 1)},
		{"null size", strings.Replace(valid, `"size_bytes":0`, `"size_bytes":null`, 1)},
		{"string size", strings.Replace(valid, `"size_bytes":0`, `"size_bytes":"0"`, 1)},
		{"negative size", strings.Replace(valid, `"size_bytes":0`, `"size_bytes":-1`, 1)},
		{"fractional size", strings.Replace(valid, `"size_bytes":0`, `"size_bytes":0.5`, 1)},
		{"decimal form", strings.Replace(valid, `"size_bytes":0`, `"size_bytes":0.0`, 1)},
		{"exponential form", strings.Replace(valid, `"size_bytes":0`, `"size_bytes":0e0`, 1)},
		{"size exceeds safe integer", strings.Replace(valid, `"size_bytes":0`, `"size_bytes":9007199254740992`, 1)},
		{"size exceeds uint64", strings.Replace(valid, `"size_bytes":0`, `"size_bytes":18446744073709551616`, 1)},
		{"duplicate size", strings.Replace(valid, "{", `{"size_bytes":0,`, 1)},
		{"duplicate shared field", strings.Replace(valid, "{", `{"entry_id":"31",`, 1)},
		{"unknown field", strings.Replace(valid, "{", `{"extra":0,`, 1)},
		{"missing digest", strings.Replace(valid, `,"sha256":"`+digest+`"`, "", 1)},
		{"null digest", strings.Replace(valid, `"`+digest+`"`, "null", 1)},
		{"case mismatched digest key", strings.Replace(valid, "sha256", "SHA256", 1)},
		{"uppercase digest", strings.Replace(valid, digest, strings.ToUpper(digest), 1)},
		{"non-hex digest", strings.Replace(valid, digest, digest[:63]+"g", 1)},
		{"short digest", strings.Replace(valid, digest, digest[:63], 1)},
		{"long digest", strings.Replace(valid, digest, digest+"0", 1)},
		{"mismatched entry ID", strings.Replace(valid, `"31"`, `"32"`, 1)},
		{"mismatched revision", strings.Replace(valid, `"2"`, `"3"`, 1)},
		{"invalid UTF-8 name", strings.Replace(valid, "note.bin", "\xff", 1)},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			event := SyncChangeEvent{
				Sequence: 3, EntryID: 31, Revision: 2, EventType: FileContentReplacedEventType, Payload: []byte(scenario.payload),
			}
			if err := ValidateSyncChangeContentEvent(event); !errors.Is(err, ErrInvalidSyncChangePage) {
				t.Fatalf("期望拒绝异常载荷，实际为 %v", err)
			}
		})
	}

	// 2. 即使载荷正确，类型也须符合六字段契约。
	for _, eventType := range []string{"ENTRY_RENAMED", "file_published", "UNKNOWN_EVENT"} {
		t.Run(eventType, func(t *testing.T) {
			event := SyncChangeEvent{
				Sequence: 3, EntryID: 31, Revision: 2, EventType: eventType, Payload: []byte(valid),
			}
			if err := ValidateSyncChangeContentEvent(event); !errors.Is(err, ErrInvalidSyncChangePage) {
				t.Fatalf("期望拒绝异常类型，实际为 %v", err)
			}
		})
	}
}
