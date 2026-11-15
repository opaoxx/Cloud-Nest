package domain

// DirectoryPageResult 保存可供响应层转换的完整目录分页结果。
type DirectoryPageResult struct {
	// Path 从真实根目录排列到本次读取的父目录。
	Path FileEntryPath
	// Entries 保存本页独立的活动子项，空页使用已初始化切片。
	Entries []FileEntry
	// NextCursor 为下一页签名文本，末页及空页为 nil。
	NextCursor *string
}
