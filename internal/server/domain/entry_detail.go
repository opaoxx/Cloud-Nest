package domain

// EntryDetail 保存同一快照中读取的可见目录项详情。
type EntryDetail struct {
	// Path 从真实根目录排列到目标，末项包含目标的基础信息。
	Path FileEntryPath
	// Display 保存目标的扩展展示字段及原始空值。
	Display FileEntryDisplay
	// Preference 保存目标的收藏及最近访问时间。
	Preference EntryPreference
}
