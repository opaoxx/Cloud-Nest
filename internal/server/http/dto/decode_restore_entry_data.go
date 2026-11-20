package dto

import "github.com/opaoxx/cloud-nest/internal/server/domain"

// DecodeRestoreEntryData 校验并解码已保存的六字段单项恢复快照。
func DecodeRestoreEntryData(body []byte) (*RestoreEntryData, error) {
	// 1. 读取唯一的 entry 对象，要求恰好六个非空字符串字段。
	fields, validFields := readEntrySnapshotFields(body, 6)
	if !validFields {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}

	// 2. 校验目录项 ID、实际父目录 ID、版本、类型和活动状态。
	entryID, validID := parsePathID(fields["id"])
	parentID, validParent := parsePathID(fields["parent_id"])
	_, validRevision := parsePathID(fields["revision"])
	entryType, status, name := fields["entry_type"], fields["status"], fields["name"]
	if !validID || !validParent || entryID == parentID || !validRevision || name == "" ||
		(entryType != "FILE" && entryType != "DIR") || status != "ACTIVE" {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}

	// 3. 返回独立的快照，供后续同键重试复用。
	return &RestoreEntryData{Entry: RestoredEntrySummary{
		ID:        fields["id"],
		ParentID:  fields["parent_id"],
		Name:      name,
		EntryType: entryType,
		Status:    status,
		Revision:  fields["revision"],
	}}, nil
}
