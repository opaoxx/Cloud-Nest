package domain

// SyncChangeSequenceBounds 保存同一账号视图中的可续读区间，两端均包含。
type SyncChangeSequenceBounds struct {
	MinimumPosition   uint64
	HighWaterSequence uint64
}
