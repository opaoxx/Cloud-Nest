package dto

import "github.com/opaoxx/cloud-nest/internal/server/domain"

// CreateDownloadRequest 保存下载授权正文的两个必填 JSON 字段。
type CreateDownloadRequest struct {
	// ExpectedRevision 是客户端读到的目录项版本。缺失或 null 为 nil。
	ExpectedRevision *string `json:"expected_revision"`
	// Purpose 是 download 或 preview。缺失或 null 为 nil。
	Purpose *string `json:"purpose"`
}

// CreateDownloadInput 保存已校验的正文参数，目标和身份分别来自路径与认证上下文。
type CreateDownloadInput struct {
	// ExpectedRevision 保留完整的非零 uint64 版本。
	ExpectedRevision uint64
	// Purpose 是已校验的 download 或 preview。
	Purpose string
}

// ParseCreateDownloadRequest 按版本、用途的顺序校验，失败时返回固定字段错误。
func ParseCreateDownloadRequest(request CreateDownloadRequest) (*CreateDownloadInput, *FieldError) {
	// 1. 校验并转换预期版本。缺失或 null 沿用空字符串的字段错误。
	var revisionText string
	if request.ExpectedRevision != nil {
		revisionText = *request.ExpectedRevision
	}
	expectedRevision, validRevision := parsePathID(revisionText)
	if !validRevision {
		return nil, &FieldError{Field: "expected_revision", Message: "预期版本须为 1～18446744073709551615 的标准十进制整数"}
	}

	// 2. 校验用途是否明确为 download 或 preview。
	if request.Purpose == nil || !domain.ValidDownloadPurpose(*request.Purpose) {
		return nil, &FieldError{Field: "purpose", Message: domain.ErrInvalidDownloadPurpose.Error()}
	}

	// 3. 返回校验后的独立参数。
	return &CreateDownloadInput{ExpectedRevision: expectedRevision, Purpose: *request.Purpose}, nil
}
