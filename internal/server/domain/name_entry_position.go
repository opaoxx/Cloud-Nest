package domain

// NameEntryPosition 保存名称键升序、目录项 ID 升序分页的内部位置。
// 位置可以指向最后扫描的隐藏候选，供扫描预算耗尽后续页。
type NameEntryPosition struct {
	// NameKey 保存 file_entries.name_key 的原始字节，按二进制顺序比较。
	NameKey []byte
	// ID 是最后扫描候选的完整无符号目录项 ID，同名键下按 ID 升序。
	ID uint64
}

// IsValid 检查非零标识和 VARBINARY(768) 名称键的非空字节长度。
func (position NameEntryPosition) IsValid() bool {
	// 1. 取得名称键的实际字节长度。
	nameKeyLength := len(position.NameKey)

	// 2. 检查非零目录项 ID 和数据库名称键容量。
	return position.ID != 0 && nameKeyLength >= 1 && nameKeyLength <= 768
}
