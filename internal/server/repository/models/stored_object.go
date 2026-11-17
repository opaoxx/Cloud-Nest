package models

import "time"

// StoredObject 是物理对象十七列的只读查询映射。
type StoredObject struct {
	// 1. 映射对象标识、归属及不可变存储位置。
	// ID 对应物理对象的完整无符号主键。
	ID uint64 `gorm:"column:id;->"`
	// OwnerUserID 对应内容所属用户。
	OwnerUserID uint64 `gorm:"column:owner_user_id;->"`
	// Provider 是服务端存储配置的逻辑标识。
	Provider string `gorm:"column:provider;->"`
	// Bucket 是对象所在的存储桶。
	Bucket string `gorm:"column:bucket;->"`
	// ObjectKey 是服务端生成的不可变对象键。
	ObjectKey string `gorm:"column:object_key;->"`
	// StorageVersionID 为 nil 时表示没有记录存储版本定位。
	StorageVersionID *string `gorm:"column:storage_version_id;->"`

	// 2. 映射大小、校验事实及只读去重生成列。
	// FileSize 是初始化预期字节数，发布前须与实际内容大小一致。
	FileSize uint64 `gorm:"column:file_size;->"`
	// VerifiedSHA256 承接可空 BINARY(32)，转换领域数组前核对长度。
	VerifiedSHA256 []byte `gorm:"column:verified_sha256;->"`
	// DedupHash 只读接收数据库生成的去重摘要。
	DedupHash []byte `gorm:"column:dedup_hash;->"`
	// StorageETag 是存储服务返回的可空不透明 ETag 原值。
	StorageETag *string `gorm:"column:storage_etag;->"`
	// VerifiedAt 是可空的内容验证完成 UTC 时间。
	VerifiedAt *time.Time `gorm:"column:verified_at;->"`

	// 3. 映射生命周期、回收时间和错误线索。
	// Status 保留数据库状态原文，转换领域记录时核对合法值。
	Status string `gorm:"column:status;->"`
	// UnreferencedAt 是可空的连续无有效引用起点 UTC 时间。
	UnreferencedAt *time.Time `gorm:"column:unreferenced_at;->"`
	// GCAfter 是可空的允许物理清理 UTC 时间。
	GCAfter *time.Time `gorm:"column:gc_after;->"`
	// LastErrorCode 是可空的恢复或清理机器错误码。
	LastErrorCode *string `gorm:"column:last_error_code;->"`

	// 4. 映射创建与更新时间。
	// CreatedAt 是对象记录创建 UTC 时间。
	CreatedAt time.Time `gorm:"column:created_at;->"`
	// UpdatedAt 是对象记录最近更新 UTC 时间。
	UpdatedAt time.Time `gorm:"column:updated_at;->"`
}

// TableName 指向既有物理对象表。
func (StoredObject) TableName() string {
	// 1. 返回既有物理对象表名。
	return "stored_objects"
}
