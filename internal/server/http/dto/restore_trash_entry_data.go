package dto

import (
	"regexp"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// trashSnapshotTimePattern 使用回收契约的 UTC 时间格式和小数秒上限。
var trashSnapshotTimePattern = regexp.MustCompile(`^[1-9][0-9]{3}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,6})?Z$`)

// RestoreTrashEntryData 校验并恢复幂等记录中的七字段回收快照。
func RestoreTrashEntryData(body []byte) (*TrashEntryData, error) {
	// 1. 检查外层对象，读取七个必填字符串。
	fields, validFields := readEntrySnapshotFields(body, 7)
	if !validFields {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}

	// 2. 校验标识、回收状态、类型和 UTC 回收时间。
	identifier, validID := parsePathID(fields["id"])
	parentID, validParent := parsePathID(fields["parent_id"])
	_, validRevision := parsePathID(fields["revision"])
	entryType, status := fields["entry_type"], fields["status"]
	if !validID || !validParent || identifier == parentID || !validRevision || fields["name"] == "" ||
		(entryType != "FILE" && entryType != "DIR") || status != "TRASHED" {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	timeText := fields["deleted_at"]
	if !trashSnapshotTimePattern.MatchString(timeText) {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	deletedAt, err := time.Parse(time.RFC3339Nano, timeText)
	if err != nil {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}

	// 3. 返回独立快照，异常统一交付幂等记录数据错误。
	return &TrashEntryData{Entry: TrashedEntrySummary{
		ID:        fields["id"],
		ParentID:  fields["parent_id"],
		Name:      fields["name"],
		EntryType: entryType,
		Status:    status,
		Revision:  fields["revision"],
		DeletedAt: deletedAt.UTC(),
	}}, nil
}
