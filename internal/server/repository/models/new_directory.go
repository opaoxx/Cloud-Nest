package models

// NewDirectory 只写入创建普通文件夹所需的七列，其余列采用数据库默认值。
// 自增 ID 在插入后通过活动名称唯一键查询取得。
type NewDirectory struct {
	// UserID 来自已认证事务绑定的用户。
	UserID uint64 `gorm:"column:user_id;<-:create"`
	// ParentID 是已经检查归属和状态的真实父目录。
	ParentID uint64 `gorm:"column:parent_id;<-:create"`
	// Name 是已经校验并去除首尾空白的展示名。
	Name string `gorm:"column:name;<-:create"`
	// NameKey 是统一名称规则生成的 UTF-8 判重键。
	NameKey []byte `gorm:"column:name_key;<-:create"`
	// EntryType 在创建目录时固定为 DIR。
	EntryType string `gorm:"column:entry_type;<-:create"`
	// Status 在创建目录时固定为 ACTIVE。
	Status string `gorm:"column:status;<-:create"`
	// ColorKey 保留文件夹颜色的 NULL、空字符串和实际值。
	ColorKey *string `gorm:"column:color_key;<-:create"`
}

// TableName 指向已有的逻辑文件与目录表。
func (NewDirectory) TableName() string { return "file_entries" }
