package domain

import (
	"errors"
	"time"
)

// ErrFileEntryNotFound 表示在指定用户范围内未找到目录项。
var ErrFileEntryNotFound = errors.New("目录项不存在")

// ErrInvalidFileEntryData 表示目录项查询结果的归属或基础标识异常。
var ErrInvalidFileEntryData = errors.New("目录项数据异常")

// FileEntry 保存文件或目录的基础记录，供后续状态与祖先链检查使用。
type FileEntry struct {
	// ID 是文件或目录的完整无符号主键。
	ID uint64
	// UserID 是目录项所属用户。
	UserID uint64
	// ParentID 为 nil 时表示真实根目录，普通项指向父目录主键。
	ParentID *uint64
	// Name 是数据库保存的展示名称，根目录名称为空字符串。
	Name string
	// EntryType 是数据库保存的 FILE 或 DIR。
	EntryType string
	// FileSize 是内容字节数快照，目录为 0，已清理文件保留审计值。
	FileSize uint64
	// Revision 是内容或命名空间变更版本。
	Revision uint64
	// Status 保留 ACTIVE、TRASHED、PURGING 或 PURGED 的生命周期事实。
	Status string
	// CreatedAt 是目录项创建时间。
	CreatedAt time.Time
	// ModifiedAt 是内容、名称、位置或回收状态最近变更时间。
	ModifiedAt time.Time
	// UpdatedAt 是数据库记录最近更新时间。
	UpdatedAt time.Time
}
