package domain

import (
	"errors"
	"time"
)

// MaxStoredObjectGCCandidates 限定一次对象回收扫描返回的数量。
const MaxStoredObjectGCCandidates = 100

// ErrStoredObjectGCCandidateStale 表示当前对象状态或计时已不符合扫描候选。
var ErrStoredObjectGCCandidateStale = errors.New("对象回收候选已失效")

// StoredObjectGCCandidate 保存扫描时的对象归属、状态和回收计时。
type StoredObjectGCCandidate struct {
	// ObjectID 和 OwnerUserID 用于在所属用户事务中重新定位对象。
	ObjectID    uint64
	OwnerUserID uint64
	// Status 是扫描时的 READY 或 ORPHANED 状态。
	Status StoredObjectStatus
	// UnreferencedAt 与 GCAfter 分别是起点和到期时间，均为 UTC。
	UnreferencedAt time.Time
	GCAfter        time.Time
}

// IsValid 核对扫描候选能否进入锁内复核。
func (candidate StoredObjectGCCandidate) IsValid() bool {
	// 1. 检查对象归属和可进入回收的状态。
	if candidate.ObjectID == 0 || candidate.OwnerUserID == 0 ||
		(candidate.Status != StoredObjectStatusReady && candidate.Status != StoredObjectStatusOrphaned) {
		return false
	}

	// 2. 检查 UTC 时间精度和完整宽限期。
	for _, stamp := range [...]time.Time{candidate.UnreferencedAt, candidate.GCAfter} {
		year := stamp.UTC().Year()
		if year < 1000 || year > 9999 || stamp.Nanosecond()%1000 != 0 {
			return false
		}
	}
	return candidate.GCAfter.Sub(candidate.UnreferencedAt) >= 7*24*time.Hour
}
