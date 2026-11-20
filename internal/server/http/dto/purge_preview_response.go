package dto

import (
	"regexp"
	"strconv"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

var purgePreviewDigestPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]{43}$`)

// validPurgePreviewDigestFormat 供签发响应和确认请求共用凭证格式约束。
func validPurgePreviewDigestFormat(digest string) bool {
	return len(digest) <= domain.MaxPurgePreviewDigestLength && purgePreviewDigestPattern.MatchString(digest)
}

// PurgePreviewData 是永久清理预检响应的七个公开字段。
type PurgePreviewData struct {
	// EntryID 和 Revision 以十进制字符串保留完整 uint64 精度。
	EntryID  string `json:"entry_id"`
	Revision string `json:"revision"`
	// FileCount 和 DirectoryCount 各自统计尚未完成清理的项。
	FileCount      int `json:"file_count"`
	DirectoryCount int `json:"directory_count"`
	// LogicalBytes 是预计释放的逻辑字节数，以十进制字符串返回。
	LogicalBytes string `json:"logical_bytes"`
	// PreviewDigest 供客户端确认时原样传回。
	PreviewDigest string `json:"preview_digest"`
	// ExpiresAt 是签名载荷中的 UTC 截止时间。
	ExpiresAt time.Time `json:"expires_at"`
}

// PurgePreviewResponse 将公开预检数据与当次请求 ID 关联。
type PurgePreviewResponse struct {
	Data      PurgePreviewData `json:"data"`
	RequestID string           `json:"request_id"`
}

// NewPurgePreviewData 将内部预检结果转换为公开响应数据。
func NewPurgePreviewData(result *domain.PurgePreview) (*PurgePreviewData, error) {
	// 1. 校验目标标识、版本与待清理数量。
	if result == nil || result.EntryID == 0 || result.Revision == 0 {
		return nil, domain.ErrInvalidFileEntryData
	}
	summary := result.Summary
	const maxCount = domain.MaxFileEntrySubtreeNodes
	if summary.FileCount < 0 || summary.FileCount > maxCount ||
		summary.DirectoryCount < 0 || summary.DirectoryCount > maxCount {
		return nil, domain.ErrInvalidFileEntryData
	}
	totalCount := summary.FileCount + summary.DirectoryCount
	if totalCount == 0 || totalCount > maxCount || (summary.FileCount == 0 && summary.LogicalBytes != 0) {
		return nil, domain.ErrInvalidFileEntryData
	}

	// 2. 校验凭证格式与到期精度，转换为 UTC。
	credential := result.Credential
	if !validPurgePreviewDigestFormat(credential.PreviewDigest) {
		return nil, domain.ErrInvalidFileEntryData
	}
	expiresAt, valid := listResponseTime(&credential.ExpiresAt)
	// 截止时间已写入凭证，转换时只调整时区，保留准确时刻。
	if !valid || !expiresAt.Equal(credential.ExpiresAt) {
		return nil, domain.ErrInvalidFileEntryData
	}

	// 3. 将标识、版本和逻辑字节数编码为十进制字符串，组装公开数据。
	return &PurgePreviewData{
		EntryID:        strconv.FormatUint(result.EntryID, 10),
		Revision:       strconv.FormatUint(result.Revision, 10),
		FileCount:      summary.FileCount,
		DirectoryCount: summary.DirectoryCount,
		LogicalBytes:   strconv.FormatUint(summary.LogicalBytes, 10),
		PreviewDigest:  credential.PreviewDigest,
		ExpiresAt:      *expiresAt,
	}, nil
}
