package dto

import "github.com/opaoxx/cloud-nest/internal/server/domain"

// RecordAccessRequest 保存记录目录项访问用途的 JSON 请求字段。
type RecordAccessRequest struct {
	// Purpose 用指针识别缺失或 null，支持 preview 和 open。
	Purpose *string `json:"purpose"`
}

// RecordAccessInput 保存校验后的访问用途，身份来自认证上下文，目标来自路径。
type RecordAccessInput struct {
	// Purpose 为 preview 时表示预览，为 open 时表示打开。
	Purpose string
}

// ParseRecordAccessRequest 校验明确的访问用途，失败返回空结果和固定字段错误。
func ParseRecordAccessRequest(request RecordAccessRequest) (*RecordAccessInput, *FieldError) {
	// 1. 检查是否明确提供支持的访问用途。
	if request.Purpose == nil || !domain.ValidEntryAccessPurpose(*request.Purpose) {
		return nil, &FieldError{Field: "purpose", Message: domain.ErrInvalidEntryAccessPurpose.Error()}
	}

	// 2. 复制用途，返回结构化访问参数。
	return &RecordAccessInput{Purpose: *request.Purpose}, nil
}
