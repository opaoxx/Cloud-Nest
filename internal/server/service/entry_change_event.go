package service

import (
	"encoding/hex"
	"encoding/json"
	"strconv"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type entryChangePayload struct {
	EntryID  string `json:"entry_id"`
	ParentID string `json:"parent_id"`
	Name     string `json:"name"`
	Revision string `json:"revision"`
}

// newEntryChangePayload 从调用方已核对的普通目录项取得当前名称与位置。
func newEntryChangePayload(entry *domain.FileEntry) entryChangePayload {
	return entryChangePayload{
		EntryID:  strconv.FormatUint(entry.ID, 10),
		ParentID: strconv.FormatUint(*entry.ParentID, 10),
		Name:     entry.Name,
		Revision: strconv.FormatUint(entry.Revision, 10),
	}
}

func newEntryChangeEvent(eventType string, entry *domain.FileEntry) (domain.ChangeEventInput, error) {
	return encodeEntryChangeEvent(eventType, entry, newEntryChangePayload(entry))
}

// newFilePublishedChangeEvent 使用实际发布条目和读回校验的内容事实。
func newFilePublishedChangeEvent(entry *domain.FileEntry, digest domain.UploadContentDigest) (domain.ChangeEventInput, error) {
	payload := struct {
		entryChangePayload
		SizeBytes uint64 `json:"size_bytes"`
		SHA256    string `json:"sha256"`
	}{
		entryChangePayload: newEntryChangePayload(entry),
		SizeBytes:          digest.SizeBytes,
		SHA256:             hex.EncodeToString(digest.SHA256[:]),
	}
	return encodeEntryChangeEvent("FILE_PUBLISHED", entry, payload)
}

func encodeEntryChangeEvent(eventType string, entry *domain.FileEntry, data any) (domain.ChangeEventInput, error) {
	payload, err := json.Marshal(data)
	if err != nil {
		return domain.ChangeEventInput{}, err
	}
	return domain.ChangeEventInput{
		EntryID: entry.ID, Revision: entry.Revision, EventType: eventType, Payload: payload,
	}, nil
}
