package dto

import (
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// CreateUploadData 保存上传创建时可交付和写入幂等记录的稳定结果。
type CreateUploadData struct {
	// Task 是创建时的十三字段公开任务快照。
	Task UploadTaskData `json:"task"`
	// UploadMode 是服务端确定的首期 SINGLE 上传方式。
	UploadMode string `json:"upload_mode"`
	// ContentPath 是携带认证凭证发送原始内容的应用接口路径。
	ContentPath string `json:"content_path"`
	// ExpiresAt 是与任务快照一致的 UTC 微秒到期时间。
	ExpiresAt time.Time `json:"expires_at"`
}

// CreateUploadResponse 关联稳定创建结果与当前 HTTP 请求 ID。
type CreateUploadResponse struct {
	// Data 保存创建时的稳定快照。
	Data CreateUploadData `json:"data"`
	// RequestID 由交付层按当前请求提供。
	RequestID string `json:"request_id"`
}

// NewCreateUploadData 转换已完成创建核对的任务，失败返回 nil 和领域错误。
// 调用方确认外层事务提交后交付响应，时间转换保留历史创建快照的到期事实。
func NewCreateUploadData(result *domain.UploadTask) (*CreateUploadData, error) {
	// 1. 检查新建 SINGLE 任务的状态和公开结果约束。
	if result == nil || result.DeviceID == 0 || result.StagedObjectID == 0 || result.UploadMode != "SINGLE" ||
		result.Status != domain.UploadStatusUploading || result.FileSize > domain.MaxSingleUploadSizeBytes || result.ReservedBytes != result.FileSize ||
		result.ResolvedName != nil || result.ResultEntryID != nil || result.CompletedAt != nil || result.LastErrorCode != nil {
		return nil, domain.ErrInvalidUploadTaskData
	}

	// 2. 复用任务响应转换，取得独立的公开快照。
	task, err := NewUploadTaskData(result)
	if err != nil {
		return nil, err
	}

	// 3. 根据任务 ID 组装内容上传路径和稳定创建结果。
	return &CreateUploadData{
		Task:        *task,
		UploadMode:  task.UploadMode,
		ContentPath: "/api/v1/uploads/" + task.ID + "/content",
		ExpiresAt:   task.ExpiresAt,
	}, nil
}
