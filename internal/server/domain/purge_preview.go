package domain

import (
	"errors"
	"time"
)

// PurgePreviewLifetime 是预检凭证从签发起的有效期。
const PurgePreviewLifetime = 5 * time.Minute

// MaxPurgePreviewDigestLength 是确认请求中签名凭证的文本上限。
const MaxPurgePreviewDigestLength = 1024

// ErrInvalidPurgePreviewDigest 表示预检凭证无效。
var ErrInvalidPurgePreviewDigest = errors.New("永久清理预检凭证无效")

// ErrPurgePreviewExpired 表示归属正确的预检凭证已到截止时间。
var ErrPurgePreviewExpired = errors.New("永久清理预检凭证已过期")

// PurgePreviewCredential 保存签发得到的凭证与对应截止时间。
type PurgePreviewCredential struct {
	// PreviewDigest 是客户端确认清理时原样传回的签名文本。
	PreviewDigest string
	// ExpiresAt 是签名载荷中相同的 UTC 微秒截止时间。
	ExpiresAt time.Time
}

// PurgePreviewSubtreeEntry 是预检统计与摘要共用的目录项事实。
type PurgePreviewSubtreeEntry struct {
	// ID 是目录项的完整无符号标识。
	ID uint64
	// EntryType 区分文件与目录。
	EntryType string
	// Status 包含全部仍保留的生命周期状态。
	Status string
	// Revision 是快照中的当前逻辑版本。
	Revision uint64
	// FileSize 是文件的逻辑大小，目录为零。
	FileSize uint64
}

// IsValid 检查可用于预检统计和摘要的目录项字段。
func (entry PurgePreviewSubtreeEntry) IsValid() bool {
	// 1. 核对标识、版本及目录大小约束。
	if entry.ID == 0 || entry.Revision == 0 ||
		(entry.EntryType != "FILE" && entry.EntryType != "DIR") || (entry.EntryType == "DIR" && entry.FileSize != 0) {
		return false
	}

	// 2. 只接受数据库约定的生命周期状态。
	switch entry.Status {
	case "ACTIVE", "TRASHED", "PURGING", "PURGED":
		return true
	default:
		return false
	}
}

// PurgePreviewSummary 保存尚未完成清理的数量和预计释放的逻辑用量。
type PurgePreviewSummary struct {
	// FileCount 是仍待处理的文件数。
	FileCount int
	// DirectoryCount 是仍待处理的目录数。
	DirectoryCount int
	// LogicalBytes 是每个仍待处理文件的逻辑字节数之和。
	LogicalBytes uint64
}
