package domain

import "time"

// MaxPurgeRootCandidates 限定一次后台扫描返回的清理根项数量。
const MaxPurgeRootCandidates = 100

// PurgeRootCandidate 保存后台扫描时看到的清理根项，供用户事务复核。
type PurgeRootCandidate struct {
	// EntryID 和 UserID 用于在所属用户事务内重新定位目标。
	EntryID uint64
	UserID  uint64
	// ParentID 是扫描时的直接父目录。
	ParentID uint64
	// Revision 是扫描时的目标版本。
	Revision uint64
	// PurgeStartedAt 是目标首次受理清理的 UTC 时间。
	PurgeStartedAt time.Time
}
