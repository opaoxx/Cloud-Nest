package dto

import (
	"strconv"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// UploadContentAcceptedData 保存文件内容已接收、等待校验的公开结果。
type UploadContentAcceptedData struct {
	// TaskID 是完整上传任务 ID 的十进制字符串。
	TaskID string `json:"task_id"`
	// Status 是内容接收后进入的 VERIFYING 阶段。
	Status string `json:"status"`
}

// UploadContentAcceptedResponse 关联接收结果与当前 HTTP 请求 ID。
type UploadContentAcceptedResponse struct {
	// Data 是可用于查询校验进度的任务结果。
	Data UploadContentAcceptedData `json:"data"`
	// RequestID 由 HTTP 交付层提供。
	RequestID string `json:"request_id"`
}

// NewUploadContentAcceptedData 将已提交到 VERIFYING 的任务转换为公开响应数据。
func NewUploadContentAcceptedData(task *domain.UploadTask) (*UploadContentAcceptedData, error) {
	// 1. 确认任务存在、ID 非零且状态为 VERIFYING。
	if task == nil || task.ID == 0 || task.Status != domain.UploadStatusVerifying {
		return nil, domain.ErrInvalidUploadTaskData
	}

	// 2. 将任务 ID 转为十进制字符串，组装公开数据。
	return &UploadContentAcceptedData{
		TaskID: strconv.FormatUint(task.ID, 10),
		Status: string(task.Status),
	}, nil
}
