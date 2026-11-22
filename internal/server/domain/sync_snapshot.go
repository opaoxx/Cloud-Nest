package domain

// SyncSnapshotHeader 保存账号一致性快照的根目录和变更截止位置。
type SyncSnapshotHeader struct {
	// RootEntryID 是账号真实活动根目录的 ID。
	RootEntryID uint64
	// HighWaterSequence 是同一读取视图中的 users.change_seq，允许为零。
	HighWaterSequence uint64
}
