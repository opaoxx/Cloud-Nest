package dto

import (
	"encoding/json"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// CompleteMultipartUploadRequest 保存首次受理或补传后再次受理的 JSON 回执。
type CompleteMultipartUploadRequest struct {
	Parts []CompleteMultipartUploadPart `json:"parts"`
}

// CompleteMultipartUploadPart 保留原始 JSON 字段，以便按数组下标定位输入错误。
type CompleteMultipartUploadPart struct {
	PartNumber json.RawMessage `json:"part_number"`
	ETag       json.RawMessage `json:"etag"`
}

// ParseCompleteMultipartUploadRequest 校验公开请求格式。具体任务计划由后续流程核对。
func ParseCompleteMultipartUploadRequest(request CompleteMultipartUploadRequest) ([]domain.UploadPartCompletionClaim, *FieldError) {
	// 1. 检查回执数量，准备存放已校验的声明。
	if len(request.Parts) < 1 || len(request.Parts) > domain.MaxUploadPartCount {
		return nil, &FieldError{Field: "parts", Message: fmt.Sprintf("完成回执须包含 1～%d 项", domain.MaxUploadPartCount)}
	}
	claims := make([]domain.UploadPartCompletionClaim, 0, len(request.Parts))
	seen := make(map[uint32]struct{}, len(request.Parts))

	// 2. 核对每项编号、重复项和 ETag，保留原顺序及大小写。
	for index, part := range request.Parts {
		var claim domain.UploadPartCompletionClaim
		if json.Unmarshal(part.PartNumber, &claim.PartNumber) != nil ||
			claim.PartNumber < 1 || claim.PartNumber > domain.MaxUploadPartCount {
			return nil, &FieldError{Field: fmt.Sprintf("parts.%d.part_number", index),
				Message: fmt.Sprintf("分片编号须为 1～%d 的整数", domain.MaxUploadPartCount)}
		}
		if _, duplicate := seen[claim.PartNumber]; duplicate {
			return nil, &FieldError{Field: "parts", Message: "完成回执的分片编号不得重复"}
		}
		if json.Unmarshal(part.ETag, &claim.ETag) != nil || !domain.ValidUploadPartETag(claim.ETag) {
			return nil, &FieldError{Field: fmt.Sprintf("parts.%d.etag", index),
				Message: "ETag 须为 1～64 位字母、数字或 . _ : -，且不含外层引号"}
		}
		seen[claim.PartNumber] = struct{}{}
		claims = append(claims, claim)
	}

	// 3. 整批通过后交付声明。
	return claims, nil
}
