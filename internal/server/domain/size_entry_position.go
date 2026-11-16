package domain

// SizeEntryPosition 保存文件大小降序、目录项 ID 降序分页的内部位置。
// 位置可以指向最后扫描的隐藏候选，供扫描预算耗尽后续页。
type SizeEntryPosition struct {
	// FileSize 保存 file_entries.file_size 的完整无符号字节数，空文件和目录为 0。
	FileSize uint64
	// ID 是最后扫描候选的完整无符号目录项 ID，同一大小下按 ID 降序。
	ID uint64
}

// IsValid 检查非零目录项标识，大小沿用 BIGINT UNSIGNED 的完整范围。
func (position SizeEntryPosition) IsValid() bool {
	// 1. 以非零 ID 确认位置有效，大小使用 uint64 的完整范围。
	return position.ID != 0
}
