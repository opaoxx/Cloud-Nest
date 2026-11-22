package dto

import (
	"crypto/sha256"
	"encoding/json"
)

// CreateUploadReplacementRequest 保留正文原值，以便按契约定位字段类型错误。
type CreateUploadReplacementRequest struct {
	ExpectedRevision json.RawMessage `json:"expected_revision"`
	SizeBytes        json.RawMessage `json:"size_bytes"`
	SHA256           json.RawMessage `json:"sha256"`
}

// CreateUploadReplacementInput 保存标准化的正文声明，目标文件 ID 由 ParseEntryPath 提供。
type CreateUploadReplacementInput struct {
	ExpectedRevision uint64
	SizeBytes        uint64
	ExpectedSHA256   [sha256.Size]byte
}

// ParseCreateUploadReplacementRequest 按契约顺序校验正文。实际版本及上传能力上限由服务比对。
func ParseCreateUploadReplacementRequest(request CreateUploadReplacementRequest) (*CreateUploadReplacementInput, *FieldError) {
	// 1. 读取预期版本，复用目录项的十进制版本校验。
	var revisionText *string
	if json.Unmarshal(request.ExpectedRevision, &revisionText) != nil {
		revisionText = nil
	}
	expectedRevision, fieldError := parseExpectedRevision(revisionText)
	if fieldError != nil {
		return nil, fieldError
	}

	// 2. 核对字节数，区分明确声明的 0 和缺失值。
	sizeBytes, fieldError := parseRequiredUploadSize(request.SizeBytes)
	if fieldError != nil {
		return nil, fieldError
	}

	// 3. 将必填的整文件 SHA-256 解码为固定摘要。
	digest, fieldError := parseRequiredUploadSHA256(request.SHA256)
	if fieldError != nil {
		return nil, fieldError
	}
	return &CreateUploadReplacementInput{
		ExpectedRevision: expectedRevision,
		SizeBytes:        sizeBytes,
		ExpectedSHA256:   digest,
	}, nil
}
