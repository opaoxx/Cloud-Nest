package domain

import (
	"errors"
	"time"
)

// DefaultFileVersionRetentionDays 是旧内容的默认保留天数。
const DefaultFileVersionRetentionDays = 30

// MaxExpiredFileVersionCandidates 是单次历史到期扫描允许返回的最大条数。
const MaxExpiredFileVersionCandidates = 100

// ErrFileVersionConflict 表示条目原版本已经存在历史记录。
var ErrFileVersionConflict = errors.New("文件历史版本已存在")

// ErrExpiredFileVersionCandidateStale 表示历史候选已消失、改变或未到期。
var ErrExpiredFileVersionCandidateStale = errors.New("到期历史版本候选已失效")

// ErrInvalidFileVersionData 表示历史版本数据不满足约束。
var ErrInvalidFileVersionData = errors.New("文件历史版本数据无效")

// FileVersion 记录被替换的旧文件版本及其计费引用。
type FileVersion struct {
	ID, UserID, EntryID, Revision, ObjectID, FileSize uint64
	CreatedAt, RetainedUntil                          time.Time
}

// IsValid 检查历史引用标识和 MySQL 微秒时间，允许零字节旧内容。
func (version FileVersion) IsValid() bool {
	if version.ID == 0 || version.UserID == 0 || version.EntryID == 0 || version.Revision == 0 || version.ObjectID == 0 {
		return false
	}
	for _, stamp := range [...]time.Time{version.CreatedAt, version.RetainedUntil} {
		year := stamp.UTC().Year()
		if year < 1000 || year > 9999 || stamp.Nanosecond()%1000 != 0 {
			return false
		}
	}
	return version.RetainedUntil.After(version.CreatedAt)
}
