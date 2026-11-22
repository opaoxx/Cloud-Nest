package domain

import (
	"errors"
	"time"
)

// DefaultFileVersionRetentionDays 是旧内容的默认保留天数。
const DefaultFileVersionRetentionDays = 30

// ErrFileVersionConflict 表示条目原版本已经存在历史记录。
var ErrFileVersionConflict = errors.New("文件历史版本已存在")

// ErrInvalidFileVersionData 表示已写入的历史行与准备保存的事实不符。
var ErrInvalidFileVersionData = errors.New("文件历史版本数据无效")

// FileVersion 记录被替换的旧文件版本及其计费引用。
type FileVersion struct {
	ID, UserID, EntryID, Revision, ObjectID, FileSize uint64
	CreatedAt, RetainedUntil                          time.Time
}
