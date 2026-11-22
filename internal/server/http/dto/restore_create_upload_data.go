package dto

import (
	"bytes"
	"encoding/json"
	"time"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// RestoreCreateUploadData 恢复并校验幂等记录中的上传创建快照。
// 结果独立持有公开数据，异常统一返回幂等记录数据错误。
func RestoreCreateUploadData(body []byte) (*CreateUploadData, error) {
	return restoreCreateUploadData(body, false)
}

func restoreCreateUploadData(body []byte, requireReplacement bool) (*CreateUploadData, error) {
	// 1. 检查外层字段、公开任务字段和初始化空值。
	if !hasCreateUploadSnapshotFields(body, requireReplacement, "task", "upload_mode", "content_path", "expires_at") {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}

	// 2. 解析数据，核对标识、创建状态和内容路径。
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

	// 3. 核对到期时间一致性，规范化时间并返回独立快照。
	if !normalizeCreateUploadSnapshotTimes(task, &data.ExpiresAt) {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	return &data, nil
}

// hasCreateUploadSnapshotFields 核对两种模式共用的初始任务结构和各自外层字段。
func hasCreateUploadSnapshotFields(body []byte, requireReplacement bool, requiredFields ...string) bool {
	// 1. 检查 JSON 编码、外层字段和必填值。
	if !utf8.Valid(body) {
		return false
	}
	readObject := func(raw []byte) (map[string]json.RawMessage, bool) {
		var fields map[string]json.RawMessage
		err := json.Unmarshal(raw, &fields)
		return fields, err == nil
	}
	if requireReplacement {
		readObject = readUniqueUploadSnapshotObject
	}
	payload, valid := readObject(body)
	if !valid || len(payload) != len(requiredFields) {
		return false
	}
	for _, name := range requiredFields {
		value, present := payload[name]
		if !present || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return false
		}
	}

	// 2. 检查任务快照字段及四项初始空值。
	fields, valid := readObject(payload["task"])
	expectedCount := 13
	if requireReplacement {
		expectedCount++
	}
	if !valid || len(fields) != expectedCount {
		return false
	}
	for _, name := range []string{
		"id", "parent_id", "requested_name", "resolved_name", "size_bytes", "upload_mode", "status",
		"result_entry_id", "created_at", "updated_at", "expires_at", "completed_at", "last_error",
	} {
		value, present := fields[name]
		if !present {
			return false
		}
		isNull := bytes.Equal(bytes.TrimSpace(value), []byte("null"))
		switch name {
		case "resolved_name", "result_entry_id", "completed_at", "last_error":
			if !isNull {
				return false
			}
		default:
			if isNull {
				return false
			}
		}
	}

	// 3. 替换快照另含原目标绑定及两个显式的空结果。
	return !requireReplacement || validCreateUploadReplacementBinding(fields["replacement"])
}

func normalizeCreateUploadSnapshotTimes(task *UploadTaskData, snapshotExpiresAt *time.Time) bool {
	// 1. 核对外层截止时间与任务一致。
	if !snapshotExpiresAt.Equal(task.ExpiresAt) {
		return false
	}
	// 2. 保留原时刻，转换为 UTC 微秒并核对年份范围。
	createdAt, validCreated := listResponseTime(&task.CreatedAt)
	updatedAt, validUpdated := listResponseTime(&task.UpdatedAt)
	expiresAt, validExpires := listResponseTime(&task.ExpiresAt)
	if !validCreated || !validUpdated || !validExpires {
		return false
	}
	task.CreatedAt, task.UpdatedAt, task.ExpiresAt = *createdAt, *updatedAt, *expiresAt
	*snapshotExpiresAt = *expiresAt
	return true
}
