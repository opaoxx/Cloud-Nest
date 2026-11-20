package domain

// PurgeProgress 保存同一只读快照中的永久清理状态。
type PurgeProgress struct {
	// EntryID 是可查询的清理目标标识。
	EntryID uint64
	// Status 是 PURGING 或 PURGED。
	Status string
	// RemainingCount 包含尚未完成的目标自身和后代。
	RemainingCount int
}
