package dto

import (
	"net/http"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const (
	// PurgeExpectedRevisionHeader 携带预检返回的目标版本。
	PurgeExpectedRevisionHeader = "X-CloudNest-Expected-Revision"
	// PurgePreviewDigestHeader 原样携带预检签发的凭证。
	PurgePreviewDigestHeader = "X-CloudNest-Purge-Preview"
)

// PurgeEntryInput 保存格式校验通过的确认清理参数。
type PurgeEntryInput struct {
	// ExpectedRevision 用于在用户事务内比较目标版本。
	ExpectedRevision uint64
	// PreviewDigest 保留凭证原文，供后续验签和计算请求摘要。
	PreviewDigest string
}

// ParsePurgeEntryHeaders 读取确认永久清理使用的两个预检请求头。
func ParsePurgeEntryHeaders(headers http.Header) (*PurgeEntryInput, *FieldError) {
	// 1. 读取唯一的版本头，校验十进制格式和范围。
	revisions := headers.Values(PurgeExpectedRevisionHeader)
	var revisionText string
	if len(revisions) == 1 {
		revisionText = revisions[0]
	}
	expectedRevision, valid := parsePathID(revisionText)
	if !valid {
		return nil, &FieldError{
			Field:   PurgeExpectedRevisionHeader,
			Message: "预期版本须为单个 1～18446744073709551615 的标准十进制整数",
		}
	}

	// 2. 读取唯一的凭证头，校验长度和格式。
	digests := headers.Values(PurgePreviewDigestHeader)
	if len(digests) != 1 || !domain.ValidPurgePreviewDigestFormat(digests[0]) {
		return nil, &FieldError{Field: PurgePreviewDigestHeader, Message: domain.ErrInvalidPurgePreviewDigest.Error()}
	}

	// 3. 返回整数版本与原样凭证。
	return &PurgeEntryInput{ExpectedRevision: expectedRevision, PreviewDigest: digests[0]}, nil
}
