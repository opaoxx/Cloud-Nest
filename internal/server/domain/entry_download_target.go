package domain

import (
	"crypto/sha256"
	"errors"
)

// ErrDownloadTargetUnavailable 表示当前文件内容缺失或不可用。
var ErrDownloadTargetUnavailable = errors.New("文件内容当前不可用")

// EntryDownloadTarget 保存同一快照中校验过的目录项和就绪对象事实。
type EntryDownloadTarget struct {
	// EntryID 是可见文件的目录项 ID。
	EntryID uint64
	// UserID 是文件所属用户。
	UserID uint64
	// Name 是签发下载时使用的文件展示名。
	Name string
	// Revision 是与客户端预期匹配的目录项版本。
	Revision uint64
	// ObjectID 是该版本引用的物理对象 ID。
	ObjectID uint64
	// SizeBytes 是目录项保存的字节数。
	SizeBytes uint64
	// MIMEType 是已保存的媒体类型，未保存时为 nil。
	MIMEType *string
	// Provider 是对象的逻辑存储配置标识。
	Provider string
	// Bucket 是对象所在的存储桶。
	Bucket string
	// ObjectKey 是定位对象的不可变存储键。
	ObjectKey string
	// StorageVersionID 是已保存的存储版本定位，缺失时为 nil。
	StorageVersionID *string
	// VerifiedSHA256 是服务端已验证并保存的内容摘要。
	VerifiedSHA256 [sha256.Size]byte
}
