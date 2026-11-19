package domain

import (
	"crypto/sha256"
	"errors"
	"time"
)

// ErrStoredObjectNotFound 表示在指定用户范围内未找到存储对象。
var ErrStoredObjectNotFound = errors.New("存储对象不存在")

// ErrInvalidStoredObjectData 表示存储对象的归属、位置或生命周期事实异常。
var ErrInvalidStoredObjectData = errors.New("存储对象数据异常")

// ErrStoredObjectLocationConflict 表示创建时的存储位置已被对象记录占用。
var ErrStoredObjectLocationConflict = errors.New("存储对象位置已存在")

// ErrStoredObjectDedupConflict 表示同用户已有相同摘要和大小的 READY 对象。
var ErrStoredObjectDedupConflict = errors.New("已有可复用的存储对象")

// ErrStoredObjectInUse 表示对象仍被有效文件或未终结上传任务引用。
var ErrStoredObjectInUse = errors.New("存储对象仍有有效引用")

// StoredObject 保存物理对象的内部记录，供上传、发布和回收流程使用。
type StoredObject struct {
	// 1. 保存对象标识、归属及不可变存储位置。
	// ID 是物理对象的完整无符号主键。
	ID uint64
	// OwnerUserID 是内容所属用户，由可信身份确定。
	OwnerUserID uint64
	// Provider 是服务端存储配置的逻辑标识。
	Provider string
	// Bucket 是对象所在的存储桶。
	Bucket string
	// ObjectKey 是服务端生成且不复用的不可变对象键。
	ObjectKey string
	// StorageVersionID 为 nil 时表示没有记录存储版本定位。
	StorageVersionID *string

	// 2. 保存内容大小、校验事实和数据库去重键。
	// FileSize 是初始化预期字节数，发布前须与实际内容大小一致。
	FileSize uint64
	// VerifiedSHA256 为 nil 时表示尚未保存服务端验证后的完整摘要。
	VerifiedSHA256 *[sha256.Size]byte
	// DedupHash 保存数据库生成列的读取结果，READY 时取已验证摘要。
	DedupHash *[sha256.Size]byte
	// StorageETag 是存储服务返回的不透明 ETag 原值，为 nil 时表示尚未记录。
	StorageETag *string
	// VerifiedAt 是可空的内容验证完成 UTC 时间。
	VerifiedAt *time.Time

	// 3. 保存生命周期状态、回收时间及错误线索。
	// Status 保存对象阶段，复用精确生命周期状态类型。
	Status StoredObjectStatus
	// UnreferencedAt 是可空的连续无有效引用起点 UTC 时间。
	UnreferencedAt *time.Time
	// GCAfter 是可空的允许物理清理 UTC 时间，与无引用起点成对保存。
	GCAfter *time.Time
	// LastErrorCode 是可空的恢复或清理机器错误码。
	LastErrorCode *string

	// 4. 保存记录的创建与更新时间。
	// CreatedAt 是对象记录创建 UTC 时间。
	CreatedAt time.Time
	// UpdatedAt 是对象记录最近更新 UTC 时间。
	UpdatedAt time.Time
}
