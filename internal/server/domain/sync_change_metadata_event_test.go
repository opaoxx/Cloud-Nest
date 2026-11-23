package domain

import (
	"errors"
	"math"
	"strings"
	"testing"
)

func TestValidateSyncChangeMetadataEvent(t *testing.T) {
	// 1. 读校验接受写入端生成的全部七种元数据载荷和完整 uint64 值。
	parentID := uint64(math.MaxUint64)
	entry := &FileEntry{ID: math.MaxUint64, ParentID: &parentID, Name: "资料 e\u0301 🚀", Revision: math.MaxUint64}
	for _, eventType := range []string{
		"DIRECTORY_CREATED", "ENTRY_RENAMED", "ENTRY_MOVED", "ENTRY_TRASHED", "ENTRY_RESTORED", EntryPurgeStartedEventType, EntryPurgedEventType,
	} {
		t.Run(eventType, func(t *testing.T) {
			written, err := NewEntryChangeEvent(eventType, entry)
			if err != nil {
				t.Fatal(err)
			}
			event := SyncChangeEvent{
				Sequence: 3, EntryID: written.EntryID, Revision: written.Revision, EventType: written.EventType, Payload: written.Payload,
			}
			if err := ValidateSyncChangeMetadataEvent(event); err != nil {
				t.Fatalf("写入端生成的事件被拒绝：%v", err)
			}
		})
	}

	// 2. 键顺序和合法 JSON 转义改变时，解码仍对应同样的值。
	event := SyncChangeEvent{
		Sequence: 3, EntryID: 12, Revision: 2, EventType: "ENTRY_RENAMED",
		Payload: []byte(" \t" + `{"revision":"\u0032","name":"e\u0301 \ud83d\ude80","parent_id":"11","entry_id":"12"}` + "\n"),
	}
	if err := ValidateSyncChangeMetadataEvent(event); err != nil {
		t.Fatalf("调序或转义后的载荷被拒绝：%v", err)
	}
}

func TestValidateSyncChangeMetadataEventRejectsMalformedData(t *testing.T) {
	// 1. 从一个合法载荷分别制造结构、文本和值的错误。
	const valid = `{"entry_id":"12","parent_id":"11","name":"目录","revision":"2"}`
	for _, scenario := range []struct {
		name    string
		payload string
	}{
		{"empty payload", ""},
		{"null object", "null"},
		{"array", "[" + valid + "]"},
		{"truncated object", strings.TrimSuffix(valid, "}")},
		{"trailing value", valid + " {}"},
		{"missing field", strings.Replace(valid, `"parent_id":"11",`, "", 1)},
		{"extra field", strings.Replace(valid, "{", `{"size_bytes":0,`, 1)},
		{"wrong case key", strings.Replace(valid, "entry_id", "Entry_ID", 1)},
		{"duplicate key", strings.Replace(valid, "{", `{"entry_id":"12",`, 1)},
		{"duplicate key with escape", strings.Replace(valid, "{", `{"\u0065ntry_id":"12",`, 1)},
		{"null field", strings.Replace(valid, `"11"`, "null", 1)},
		{"numeric identifier", strings.Replace(valid, `"11"`, "11", 1)},
		{"noncanonical parent", strings.Replace(valid, "11", "011", 1)},
		{"zero parent", strings.Replace(valid, "11", "0", 1)},
		{"parent overflow", strings.Replace(valid, "11", "18446744073709551616", 1)},
		{"entry mismatch", strings.Replace(valid, "12", "13", 1)},
		{"noncanonical entry", strings.Replace(valid, "12", "012", 1)},
		{"revision mismatch", strings.Replace(valid, `"2"`, `"3"`, 1)},
		{"noncanonical revision", strings.Replace(valid, `"2"`, `"02"`, 1)},
		{"empty name", strings.Replace(valid, "目录", "", 1)},
		{"object instead of name", strings.Replace(valid, `"目录"`, "{}", 1)},
		{"invalid UTF-8", strings.Replace(valid, "目录", "\xff", 1)},
		{"unpaired surrogate", strings.Replace(valid, "目录", `\ud800`, 1)},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			event := SyncChangeEvent{
				Sequence: 3, EntryID: 12, Revision: 2, EventType: "ENTRY_RENAMED", Payload: []byte(scenario.payload),
			}
			if err := ValidateSyncChangeMetadataEvent(event); !errors.Is(err, ErrInvalidSyncChangePage) {
				t.Fatalf("期望拒绝异常载荷，实际为 %v", err)
			}
			if string(event.Payload) != scenario.payload {
				t.Fatal("校验改写了事件载荷")
			}
		})
	}

	// 2. 外层标识须非零，类型须与载荷的四字段契约相符。
	for _, scenario := range []struct {
		name              string
		eventType         string
		entryID, revision uint64
	}{
		{"zero outer entry ID", "ENTRY_RENAMED", 0, 2},
		{"zero outer revision", "ENTRY_RENAMED", 12, 0},
		{"unknown event type", "UNKNOWN_EVENT", 12, 2},
		{"wrong event type case", "entry_renamed", 12, 2},
		{"publication event", "FILE_PUBLISHED", 12, 2},
		{"replacement event", FileContentReplacedEventType, 12, 2},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			event := SyncChangeEvent{
				Sequence: 3, EntryID: scenario.entryID, Revision: scenario.revision, EventType: scenario.eventType, Payload: []byte(valid),
			}
			if err := ValidateSyncChangeMetadataEvent(event); !errors.Is(err, ErrInvalidSyncChangePage) {
				t.Fatalf("期望拒绝异常事件，实际为 %v", err)
			}
		})
	}
}
