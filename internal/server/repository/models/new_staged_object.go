package models

// NewStagedObject 映射创建暂存对象所需的六列，其余列采用数据库默认值。
// 插入后的对象标识通过存储位置唯一键回读，保留完整无符号范围。
type NewStagedObject struct {
	// 1. 映射可信用户归属和服务端存储位置。
	// OwnerUserID 来自已认证用户事务绑定的身份。
	OwnerUserID uint64 `gorm:"column:owner_user_id;<-:create"`
	// Provider 来自已校验的服务端存储配置标识。
	Provider string `gorm:"column:provider;<-:create"`
	// Bucket 来自已校验的服务端存储桶配置。
	Bucket string `gorm:"column:bucket;<-:create"`
	// ObjectKey 是服务端为本次尝试生成的不可变对象键。
	ObjectKey string `gorm:"column:object_key;<-:create"`

	// 2. 映射声明大小和初始暂存状态。
	// FileSize 来自创建输入的完整无符号 SizeBytes，零字节有效。
	FileSize uint64 `gorm:"column:file_size;<-:create"`
	// Status 由暂存对象写入流程固定为 STAGING。
	Status string `gorm:"column:status;<-:create"`
}

// TableName 指向既有物理对象表。
func (NewStagedObject) TableName() string {
	// 1. 返回既有物理对象表名。
	return "stored_objects"
}
