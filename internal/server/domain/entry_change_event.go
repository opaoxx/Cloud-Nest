package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
)

// EntryPurgeStartedEventType 表示回收站根项已进入清理流程。
const EntryPurgeStartedEventType = "ENTRY_PURGE_STARTED"

// EntryPurgedEventType 表示条目已完成逻辑清理。
const EntryPurgedEventType = "ENTRY_PURGED"

type entryChangePayload struct {
	EntryID  string `json:"entry_id"`
	ParentID string `json:"parent_id"`
	Name     string `json:"name"`
	Revision string `json:"revision"`
}

// newEntryChangePayload 从调用方已核对的普通目录项取得当前名称与位置。
func newEntryChangePayload(entry *FileEntry) entryChangePayload {
	return entryChangePayload{
		EntryID:  strconv.FormatUint(entry.ID, 10),
		ParentID: strconv.FormatUint(*entry.ParentID, 10),
		Name:     entry.Name,
		Revision: strconv.FormatUint(entry.Revision, 10),
	}
}

// NewEntryChangeEvent 编码调用方已核对的普通条目状态。
func NewEntryChangeEvent(eventType string, entry *FileEntry) (ChangeEventInput, error) {
	return encodeEntryChangeEvent(eventType, entry, newEntryChangePayload(entry))
}

// NewFilePublishedChangeEvent 使用实际发布条目和已核对的内容摘要。
func NewFilePublishedChangeEvent(entry *FileEntry, digest [sha256.Size]byte) (ChangeEventInput, error) {
	payload := struct {
		entryChangePayload
		SizeBytes uint64 `json:"size_bytes"`
		SHA256    string `json:"sha256"`
	}{
		entryChangePayload: newEntryChangePayload(entry),
		SizeBytes:          entry.FileSize,
		SHA256:             hex.EncodeToString(digest[:]),
	}
	return encodeEntryChangeEvent("FILE_PUBLISHED", entry, payload)
}

func encodeEntryChangeEvent(eventType string, entry *FileEntry, data any) (ChangeEventInput, error) {
	payload, err := json.Marshal(data)
	if err != nil {
		return ChangeEventInput{}, err
	}
	return ChangeEventInput{
		EntryID: entry.ID, Revision: entry.Revision, EventType: eventType, Payload: payload,
	}, nil
}
