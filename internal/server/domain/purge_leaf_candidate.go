package domain

// MaxPurgeLeafCandidates 限定单个用户事务选择的清理叶子数量。
const MaxPurgeLeafCandidates = 100

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
