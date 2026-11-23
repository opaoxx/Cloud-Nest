package domain

import (
	jsonv2 "encoding/json/v2"
	"fmt"
	"strconv"
)

// ValidateSyncChangeMetadataEvent 核对四字段元数据事件的类型和载荷。
func ValidateSyncChangeMetadataEvent(event SyncChangeEvent) error {
	// 1. 确认事件属于四字段元数据类型。
	switch event.EventType {
	case "DIRECTORY_CREATED", "ENTRY_RENAMED", "ENTRY_MOVED", "ENTRY_TRASHED", "ENTRY_RESTORED", EntryPurgeStartedEventType, EntryPurgedEventType:
	default:
		return fmt.Errorf("%w: 四字段载荷的事件类型无效", ErrInvalidSyncChangePage)
	}

	// 2. 严格解码 JSON，拒绝未知字段、重复字段和无效文本。
	var payload entryChangePayload
	if err := jsonv2.Unmarshal(event.Payload, &payload, jsonv2.RejectUnknownMembers(true)); err != nil {
		return fmt.Errorf("%w: 四字段事件载荷格式无效", ErrInvalidSyncChangePage)
	}

	// 3. 校验名称和 ID 格式，并核对条目 ID、版本与外层一致。
	return validateSyncChangeEntryPayload(event, payload)
}

// validateSyncChangeEntryPayload 核对元数据和内容事件共有的四个字段。
func validateSyncChangeEntryPayload(event SyncChangeEvent, payload entryChangePayload) error {
	if event.EntryID == 0 || event.Revision == 0 ||
		payload.EntryID != strconv.FormatUint(event.EntryID, 10) ||
		payload.Revision != strconv.FormatUint(event.Revision, 10) {
		return fmt.Errorf("%w: 载荷条目 ID 或版本与事件不符", ErrInvalidSyncChangePage)
	}
	parentID, err := strconv.ParseUint(payload.ParentID, 10, 64)
	if err != nil || parentID == 0 || strconv.FormatUint(parentID, 10) != payload.ParentID || payload.Name == "" {
		return fmt.Errorf("%w: 载荷父目录 ID 或名称无效", ErrInvalidSyncChangePage)
	}
	return nil
}
