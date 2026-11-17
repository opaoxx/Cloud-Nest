package dto

import (
	"bytes"
	"encoding/json"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// RestoreCreateUploadData 恢复并校验幂等记录中的上传创建快照。
// 结果独立持有公开数据，异常统一返回幂等记录数据错误。
func RestoreCreateUploadData(body []byte) (*CreateUploadData, error) {
	// 1. 检查 JSON 编码、外层字段和必填值。
	var payload map[string]json.RawMessage
	if !utf8.Valid(body) || json.Unmarshal(body, &payload) != nil || len(payload) != 4 {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	for _, name := range []string{"task", "upload_mode", "content_path", "expires_at"} {
		value, present := payload[name]
		if !present || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, domain.ErrInvalidIdempotencyRecordData
		}
	}

	// 2. 检查任务快照字段及四项初始空值。
	var fields map[string]json.RawMessage
	if json.Unmarshal(payload["task"], &fields) != nil || len(fields) != 13 {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	for _, name := range []string{
		"id", "parent_id", "requested_name", "resolved_name", "size_bytes", "upload_mode", "status",
		"result_entry_id", "created_at", "updated_at", "expires_at", "completed_at", "last_error",
	} {
		value, present := fields[name]
		if !present {
			return nil, domain.ErrInvalidIdempotencyRecordData
		}
		isNull := bytes.Equal(bytes.TrimSpace(value), []byte("null"))
		switch name {
		case "resolved_name", "result_entry_id", "completed_at", "last_error":
			if !isNull {
				return nil, domain.ErrInvalidIdempotencyRecordData
			}
		default:
			if isNull {
				return nil, domain.ErrInvalidIdempotencyRecordData
			}
		}
	}

	// 3. 解析数据，核对标识、创建状态和内容路径。
	var data CreateUploadData
	if json.Unmarshal(body, &data) != nil {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	task := &data.Task
	_, validID := parsePathID(task.ID)
	_, validParent := parsePathID(task.ParentID)
	if !validID || !validParent || task.RequestedName == "" || !utf8.ValidString(task.RequestedName) ||
		task.SizeBytes > domain.MaxSingleUploadSizeBytes || task.UploadMode != "SINGLE" || task.Status != string(domain.UploadStatusUploading) ||
		data.UploadMode != task.UploadMode || data.ContentPath != "/api/v1/uploads/"+task.ID+"/content" {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}

	// 4. 核对到期时间一致性，规范化时间并返回独立快照。
	if !data.ExpiresAt.Equal(task.ExpiresAt) {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	createdAt, validCreated := listResponseTime(&task.CreatedAt)
	updatedAt, validUpdated := listResponseTime(&task.UpdatedAt)
	expiresAt, validExpires := listResponseTime(&task.ExpiresAt)
	if !validCreated || !validUpdated || !validExpires {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	task.CreatedAt, task.UpdatedAt, task.ExpiresAt = *createdAt, *updatedAt, *expiresAt
	data.ExpiresAt = *expiresAt
	return &data, nil
}
