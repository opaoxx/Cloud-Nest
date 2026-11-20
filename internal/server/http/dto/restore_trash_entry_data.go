package dto

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// trashSnapshotTimePattern 使用回收契约的 UTC 时间格式和小数秒上限。
var trashSnapshotTimePattern = regexp.MustCompile(`^[1-9][0-9]{3}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,6})?Z$`)

// RestoreTrashEntryData 校验并恢复幂等记录中的七字段回收快照。
func RestoreTrashEntryData(body []byte) (*TrashEntryData, error) {
	// 1. 检查外层对象，读取七个必填字符串。
	fields, validFields := readTrashSnapshotFields(body)
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

// readTrashSnapshotFields 按解码后的键读取恰好七项字段，拒绝重复键。
func readTrashSnapshotFields(body []byte) (map[string]string, bool) {
	// 1. 检查 UTF-8 与只有 entry 对象的外层。
	if !utf8.Valid(body) {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	for _, expected := range []json.Token{json.Delim('{'), "entry", json.Delim('{')} {
		token, err := decoder.Token()
		if err != nil || token != expected {
			return nil, false
		}
	}

	// 2. 允许字段调序，每项只接受一个非空字符串。
	const fieldCount = 7
	fields := make(map[string]string, fieldCount)
	for decoder.More() {
		nameToken, nameError := decoder.Token()
		name, validName := nameToken.(string)
		if _, duplicate := fields[name]; nameError != nil || !validName || duplicate || len(fields) == fieldCount {
			return nil, false
		}
		valueToken, valueError := decoder.Token()
		value, validValue := valueToken.(string)
		if valueError != nil || !validValue || value == "" {
			return nil, false
		}
		fields[name] = value
	}
	if len(fields) != fieldCount {
		return nil, false
	}

	// 3. 检查两个对象完整闭合，正文到此结束。
	for range 2 {
		token, err := decoder.Token()
		if err != nil || token != json.Delim('}') {
			return nil, false
		}
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, false
	}
	return fields, true
}
