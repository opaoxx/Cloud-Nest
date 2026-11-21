package dto

import (
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

var uploadPartRenewalTimePattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,6})?Z$`)

// UploadPartURLsRequest 保存分片签发的 JSON 声明和可选旧期限。
type UploadPartURLsRequest struct {
	Parts            []UploadPartURLRequestItem `json:"parts"`
	RenewIfExpiresAt json.RawMessage            `json:"renew_if_expires_at"`
}

// UploadPartURLRequestItem 保留字段的 JSON 原值，便于定位到错误分片的编号或摘要。
type UploadPartURLRequestItem struct {
	PartNumber json.RawMessage `json:"part_number"`
	SHA256     json.RawMessage `json:"sha256"`
}

// UploadPartURLsInput 保存已校验格式的声明，ExpectedExpiry 为空时表示普通签发。
type UploadPartURLsInput struct {
	Parts          []domain.UploadPartDeclaration
	ExpectedExpiry *time.Time
}

// ParseUploadPartURLsRequest 转换 HTTP 声明。具体编号是否属于任务，由服务读取保存的计划后核对。
func ParseUploadPartURLsRequest(request UploadPartURLsRequest) (*UploadPartURLsInput, *FieldError) {
	// 1. 检查整批数量、编号和摘要，按声明顺序保存。
	if len(request.Parts) < 1 || len(request.Parts) > domain.MaxUploadPartSigningInputs {
		return nil, &FieldError{Field: "parts", Message: fmt.Sprintf("分片声明须包含 1～%d 项", domain.MaxUploadPartSigningInputs)}
	}
	input := &UploadPartURLsInput{Parts: make([]domain.UploadPartDeclaration, len(request.Parts))}
	seen := make(map[uint32]struct{}, len(request.Parts))
	for index, part := range request.Parts {
		var declaration domain.UploadPartDeclaration
		if json.Unmarshal(part.PartNumber, &declaration.PartNumber) != nil ||
			declaration.PartNumber < 1 || declaration.PartNumber > domain.MaxUploadPartCount {
			return nil, &FieldError{Field: fmt.Sprintf("parts.%d.part_number", index),
				Message: fmt.Sprintf("分片编号须为 1～%d 的整数", domain.MaxUploadPartCount)}
		}
		if _, duplicate := seen[declaration.PartNumber]; duplicate {
			return nil, &FieldError{Field: "parts", Message: "同批分片编号不得重复"}
		}
		seen[declaration.PartNumber] = struct{}{}
		decodeError := json.Unmarshal(part.SHA256, &declaration.SHA256)
		if _, valid := domain.DecodeUploadPartSHA256(declaration.SHA256); decodeError != nil || !valid {
			return nil, &FieldError{Field: fmt.Sprintf("parts.%d.sha256", index), Message: "SHA-256 须为 64 位十六进制文本"}
		}
		input.Parts[index] = declaration
	}

	// 2. 仅省略字段才使用普通签发。字段存在时必须提供合法的时间字符串。
	if len(request.RenewIfExpiresAt) == 0 {
		return input, nil
	}
	timeError := &FieldError{Field: "renew_if_expires_at", Message: "旧截止时间须为有效的 UTC 时间，以 Z 结尾且最多六位小数"}
	var expiryText string
	if json.Unmarshal(request.RenewIfExpiresAt, &expiryText) != nil || !uploadPartRenewalTimePattern.MatchString(expiryText) {
		return nil, timeError
	}
	expiry, err := time.Parse(time.RFC3339Nano, expiryText)
	if err != nil {
		return nil, timeError
	}

	// 3. 返回精确的 UTC 时刻，供事务比较客户端看到的原期限。
	input.ExpectedExpiry = &expiry
	return input, nil
}
