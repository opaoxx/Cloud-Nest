package domain

import "time"

// RecentEntryCandidate 保存最近使用候选的基础记录和当前用户的访问时间。
type RecentEntryCandidate struct {
	// Entry 是活动普通文件，完整祖先链可见性由上层在同一快照内检查。
	Entry FileEntry
	// LastOpenedAt 来自当前用户的非空访问偏好，供排序及续页位置使用。
	LastOpenedAt time.Time
}

// RecentEntryPage 保存按最近访问时间、ID 降序读取的候选页。
type RecentEntryPage struct {
	// Entries 在空页时使用初始化后的空切片。
	Entries []RecentEntryCandidate
	// HasMore 表示本次快照中当前页后还有候选。
	HasMore bool
}
