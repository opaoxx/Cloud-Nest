package dto

import (
	"encoding/json"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// EncodeCreateUploadSnapshot 保存新建上传任务的公开结果，供幂等重放。
func EncodeCreateUploadSnapshot(task *domain.UploadTask) (json.RawMessage, error) {
	// 1. 根据任务模式复用对应的公开字段和初始状态校验。
	if task != nil && task.UploadMode == "MULTIPART" {
		data, err := NewCreateMultipartUploadData(task)
		if err != nil {
			return nil, err
		}
		return json.Marshal(data)
	}

	// 2. SINGLE 沿用已有字段和 JSON 编码，历史快照仍可重放。
	data, err := NewCreateUploadData(task)
	if err != nil {
		return nil, err
	}
	return json.Marshal(data)
}

// ValidateCreateUploadSnapshot 按保存时的模式核对完整快照，不重新计算分片计划。
func ValidateCreateUploadSnapshot(body json.RawMessage) error {
	// 1. 读取快照模式，确定使用哪一套完整字段校验。
	var snapshot struct {
		UploadMode string `json:"upload_mode"`
	}
	if err := json.Unmarshal(body, &snapshot); err != nil {
		return domain.ErrInvalidIdempotencyRecordData
	}

	// 2. 复用已有恢复校验，保留已保存的 ID、计划和到期时间。
	if snapshot.UploadMode == "MULTIPART" {
		_, err := RestoreCreateMultipartUploadData(body)
		return err
	}
	_, err := RestoreCreateUploadData(body)
	return err
}
