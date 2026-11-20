package domain

import (
	"errors"
	"time"
)

// TrashRetentionDays 是回收站展示与到期扫描共用的保留天数。
const TrashRetentionDays = 30

// MaxExpiredTrashRootCandidates 限定一次到期根项扫描返回的数量。
const MaxExpiredTrashRootCandidates = 100

// ErrExpiredTrashRootCandidateStale 表示候选已不符合当前到期根项条件。
var ErrExpiredTrashRootCandidateStale = errors.New("到期回收站根项候选已失效")

// ExpiredTrashRootCandidate 保存扫描时的归属、原父目录、版本与回收时间。
type ExpiredTrashRootCandidate struct {
	EntryID   uint64
	UserID    uint64
	ParentID  uint64
	Revision  uint64
	DeletedAt time.Time
}

// IsValid 校验可用于后续事务复核的候选字段。
func (candidate ExpiredTrashRootCandidate) IsValid() bool {
	// 1. 核对归属、父目录和版本。
	if candidate.UserID == 0 || candidate.ParentID == 0 || candidate.ParentID == candidate.EntryID || candidate.Revision == 0 {
		return false
	}

	// 2. 复用回收时间的数据库精度检查。
	return (TrashEntryPosition{ID: candidate.EntryID, DeletedAt: candidate.DeletedAt}).IsValid()
}
