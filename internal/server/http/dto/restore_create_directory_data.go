package dto

import (
	"bytes"
	"encoding/json"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// RestoreCreateDirectoryData 恢复幂等记录中的稳定创建快照。
// 结果独立持有字段，时间沿用 UTC 微秒规则，异常统一返回幂等记录数据错误。
func RestoreCreateDirectoryData(body []byte) (*CreateDirectoryData, error) {
	// 1. 检查 JSON 编码和稳定数据外层。
	var payload map[string]json.RawMessage
	if !utf8.Valid(body) || json.Unmarshal(body, &payload) != nil || len(payload) != 1 || payload["entry"] == nil {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}

	// 2. 检查目录快照的必需字段及空值。
	var fields map[string]json.RawMessage
	if json.Unmarshal(payload["entry"], &fields) != nil || len(fields) != 11 {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	for _, name := range []string{
		"id", "parent_id", "name", "entry_type", "size_bytes", "revision",
		"created_at", "modified_at", "updated_at", "status", "color_key",
	} {
		value, present := fields[name]
		if !present || (name != "color_key" && bytes.Equal(bytes.TrimSpace(value), []byte("null"))) {
			return nil, domain.ErrInvalidIdempotencyRecordData
		}
	}

	// 3. 解析字段类型，核对标识和新建目录规则。
	var data CreateDirectoryData
	if json.Unmarshal(body, &data) != nil {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	entry := &data.Entry
	identifier, validID := parsePathID(entry.ID)
	parentID, validParent := parsePathID(entry.ParentID)
	if !validID || !validParent || identifier == parentID || entry.Name == "" || !utf8.ValidString(entry.Name) ||
		entry.EntryType != "DIR" || entry.SizeBytes != 0 || entry.Revision != "1" || entry.Status != "ACTIVE" ||
		!domain.ValidDirectoryColorKey(entry.ColorKey) {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}

	// 4. 规范化时间并返回独立的创建快照。
	createdAt, validCreated := listResponseTime(&entry.CreatedAt)
	modifiedAt, validModified := listResponseTime(&entry.ModifiedAt)
	updatedAt, validUpdated := listResponseTime(&entry.UpdatedAt)
	if !validCreated || !validModified || !validUpdated {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	entry.CreatedAt, entry.ModifiedAt, entry.UpdatedAt = *createdAt, *modifiedAt, *updatedAt
	return &data, nil
}
