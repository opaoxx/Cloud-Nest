package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"unicode/utf8"
)

// EntryPurgeStartedEventType 表示回收站根项已进入清理流程。
const EntryPurgeStartedEventType = "ENTRY_PURGE_STARTED"

// EntryPurgedEventType 表示条目已完成逻辑清理。
const EntryPurgedEventType = "ENTRY_PURGED"

// FileContentReplacedEventType 表示既有文件已发布新的内容版本。
const FileContentReplacedEventType = "FILE_CONTENT_REPLACED"

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
	return newFileContentChangeEvent("FILE_PUBLISHED", entry, digest)
}

// NewFileContentReplacedChangeEvent 从已替换的文件和实测内容构造变更事实。
func NewFileContentReplacedChangeEvent(entry *FileEntry, digest UploadContentDigest) (ChangeEventInput, error) {
	// 1. 核对替换后的活动文件、版本和已校验字节数。
	if entry == nil || entry.ID == 0 || entry.ParentID == nil || *entry.ParentID == 0 ||
		entry.EntryType != "FILE" || entry.Status != "ACTIVE" || entry.Revision < 2 ||
		entry.Name == "" || !utf8.ValidString(entry.Name) || entry.FileSize != digest.SizeBytes {
		return ChangeEventInput{}, ErrInvalidChangeEventInput
	}

	// 2. 沿用文件发布的载荷编码，以事件类型标识既有文件内容发生替换。
	return newFileContentChangeEvent(FileContentReplacedEventType, entry, digest.SHA256)
}

func newFileContentChangeEvent(eventType string, entry *FileEntry, digest [sha256.Size]byte) (ChangeEventInput, error) {
	payload := struct {
		entryChangePayload
		SizeBytes uint64 `json:"size_bytes"`
		SHA256    string `json:"sha256"`
	}{
		entryChangePayload: newEntryChangePayload(entry),
		SizeBytes:          entry.FileSize,
		SHA256:             hex.EncodeToString(digest[:]),
	}
	return encodeEntryChangeEvent(eventType, entry, payload)
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
