package dto

import (
	"bytes"
	"encoding/json"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// restoreEntrySnapshot 校验普通活动目录项的完整详情快照，供业务恢复入口共用。
func restoreEntrySnapshot(body []byte) (*EntryDetailData, error) {
	// 1. 检查 JSON 编码和稳定数据外层。
	var payload map[string]json.RawMessage
	if !utf8.Valid(body) || json.Unmarshal(body, &payload) != nil || len(payload) != 1 || payload["entry"] == nil {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}

	// 2. 检查十八项字段、必需值及来源设备结构。
	var fields map[string]json.RawMessage
	if json.Unmarshal(payload["entry"], &fields) != nil || len(fields) != 18 {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	for _, name := range []string{
		"id", "parent_id", "name", "kind", "size_bytes", "revision", "created_at", "modified_at", "updated_at", "status",
		"extension", "mime_type", "color_key", "content_modified_at", "source_device", "starred", "starred_at", "last_opened_at",
	} {
		value, present := fields[name]
		if !present {
			return nil, domain.ErrInvalidIdempotencyRecordData
		}
		switch name {
		case "extension", "mime_type", "color_key", "content_modified_at", "source_device", "starred_at", "last_opened_at":
		default:
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return nil, domain.ErrInvalidIdempotencyRecordData
			}
		}
	}
	if !bytes.Equal(bytes.TrimSpace(fields["source_device"]), []byte("null")) {
		var sourceFields map[string]json.RawMessage
		if json.Unmarshal(fields["source_device"], &sourceFields) != nil || len(sourceFields) != 2 || sourceFields["id"] == nil || sourceFields["name"] == nil {
			return nil, domain.ErrInvalidIdempotencyRecordData
		}
	}

	// 3. 解析字段，核对标识、类别和快照规则。
	var data struct {
		Entry EntryDetailData `json:"entry"`
	}
	if json.Unmarshal(body, &data) != nil {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	entry := &data.Entry
	identifier, validID := parsePathID(entry.ID)
	if entry.ParentID == nil {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	parentID, validParent := parsePathID(*entry.ParentID)
	_, validRevision := parsePathID(entry.Revision)
	if !validID || !validParent || identifier == parentID || !validRevision || entry.Name == "" || !utf8.ValidString(entry.Name) ||
		entry.SizeBytes > maxJSONSafeInteger || entry.Status != "ACTIVE" || entry.Starred != (entry.StarredAt != nil) {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	switch entry.Kind {
	case "folder":
		if entry.SizeBytes != 0 {
			return nil, domain.ErrInvalidIdempotencyRecordData
		}
	case "document", "image", "video", "audio", "archive", "other":
	default:
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	if source := entry.SourceDevice; source != nil {
		if source.ID == nil && source.Name == nil {
			return nil, domain.ErrInvalidIdempotencyRecordData
		}
		if source.ID != nil {
			if _, valid := parsePathID(*source.ID); !valid {
				return nil, domain.ErrInvalidIdempotencyRecordData
			}
		}
	}

	// 4. 规范化时间，返回独立的目录项快照。
	createdAt, validCreated := listResponseTime(&entry.CreatedAt)
	modifiedAt, validModified := listResponseTime(&entry.ModifiedAt)
	updatedAt, validUpdated := listResponseTime(&entry.UpdatedAt)
	contentAt, validContent := listResponseTime(entry.ContentModifiedAt)
	starredAt, validStarred := listResponseTime(entry.StarredAt)
	openedAt, validOpened := listResponseTime(entry.LastOpenedAt)
	if !validCreated || !validModified || !validUpdated || !validContent || !validStarred || !validOpened {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	entry.CreatedAt, entry.ModifiedAt, entry.UpdatedAt = *createdAt, *modifiedAt, *updatedAt
	entry.ContentModifiedAt, entry.StarredAt, entry.LastOpenedAt = contentAt, starredAt, openedAt
	return entry, nil
}
