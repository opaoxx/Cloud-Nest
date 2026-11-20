package domain

import "errors"

// MaxPurgeLeafCandidates 限定单个用户事务选择的清理叶子数量。
const MaxPurgeLeafCandidates = 100

// ErrPurgeLeafCandidateStale 表示候选已离开原范围或不再具备叶子条件。
var ErrPurgeLeafCandidateStale = errors.New("永久清理叶子候选已失效")

// PurgeLeafCandidate 保存当前清理根项内可处理的目录项。
// 所属用户与根项由产生候选的事务限定。
type PurgeLeafCandidate struct {
	// EntryID 和 ParentID 标识目标及当前直接父目录。
	EntryID  uint64
	ParentID uint64
	// EntryType 区分文件和目录。
	EntryType string
	// Status 与 Revision 供后续条件写入复核。
	Status   string
	Revision uint64
	// FileSize 是后续结算所需的逻辑字节数，目录为零。
	FileSize uint64
}

// IsValid 检查候选能否进入锁内复核。
func (candidate PurgeLeafCandidate) IsValid() bool {
	// 1. 复用子树记录的标识、类型、大小和状态校验。
	entry := PurgePreviewSubtreeEntry{
		ID: candidate.EntryID, EntryType: candidate.EntryType, Status: candidate.Status,
		Revision: candidate.Revision, FileSize: candidate.FileSize,
	}
	return entry.IsValid() && entry.Status != "PURGED" && candidate.ParentID != 0 && candidate.ParentID != candidate.EntryID
}

// LockedPurgeLeaf 是锁内确认仍可处理的叶子与当前内容引用。
type LockedPurgeLeaf struct {
	PurgeLeafCandidate
	// UserID 是锁定记录的所属用户。
	UserID uint64
	// ObjectID 是文件的当前内容引用，目录为 nil。
	ObjectID *uint64
}
