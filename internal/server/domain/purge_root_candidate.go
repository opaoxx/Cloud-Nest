package domain

import (
	"errors"
	"time"
)

// MaxPurgeRootCandidates 限定一次后台扫描返回的清理根项数量。
const MaxPurgeRootCandidates = 100

// ErrPurgeRootCandidateStale 表示扫描到的根项已不符合当前清理条件。
var ErrPurgeRootCandidateStale = errors.New("永久清理根项候选已失效")

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

// IsValid 校验后台候选的标识、版本和数据库时间精度。
func (candidate PurgeRootCandidate) IsValid() bool {
	// 1. 核对非零标识和版本，排除真实根目录及指向自身的父目录。
	if candidate.EntryID == 0 || candidate.UserID == 0 || candidate.ParentID == 0 ||
		candidate.ParentID == candidate.EntryID || candidate.Revision == 0 {
		return false
	}

	// 2. 核对 UTC 年份范围与 DATETIME(6) 的微秒精度。
	startedAt := candidate.PurgeStartedAt.UTC()
	return startedAt.Year() >= 1000 && startedAt.Year() <= 9999 && startedAt.Nanosecond()%1000 == 0
}
