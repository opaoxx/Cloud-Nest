package models

import "time"

// UploadTask 是上传任务二十八列的只读查询映射。
type UploadTask struct {
	// 1. 映射任务标识、归属及当前暂存对象。
	// ID 对应上传任务的完整无符号主键。
	ID uint64 `gorm:"column:id;->"`
	// UserID 是任务所属用户，由可信身份确定。
	UserID uint64 `gorm:"column:user_id;->"`
	// DeviceID 是创建任务时的来源设备。
	DeviceID uint64 `gorm:"column:device_id;->"`
	// StagedObjectID 是当前上传尝试使用的暂存对象。
	StagedObjectID uint64 `gorm:"column:staged_object_id;->"`

	// 2. 映射目标目录、声明及分片信息。
	// TargetParentID 是任务准备发布到的目标目录。
	TargetParentID uint64 `gorm:"column:target_parent_id;->"`
	// RequestedName 是通过统一命名校验的请求展示名。
	RequestedName string `gorm:"column:requested_name;->"`
	// UploadMode 保存服务端确定的 SINGLE 或 MULTIPART。
	UploadMode string `gorm:"column:upload_mode;->"`
	// StorageUploadID 为 nil 时表示没有对象存储分片会话。
	StorageUploadID *string `gorm:"column:storage_upload_id;->"`
	// ExpectedSHA256 承接可空 BINARY(32)，转换领域数组前核对实际长度。
	ExpectedSHA256 []byte `gorm:"column:expected_sha256;->"`
	// FileSize 是声明的完整无符号字节数。
	FileSize uint64 `gorm:"column:file_size;->"`
	// PartSize 为 nil 时表示没有分片大小规划。
	PartSize *uint64 `gorm:"column:part_size;->"`
	// PartCount 为 nil 时表示没有分片数量规划，对齐 INT UNSIGNED。
	PartCount *uint32 `gorm:"column:part_count;->"`

	// 3. 映射任务状态、发布结果及剩余预占。
	// Status 保留数据库状态原文，转换领域记录时核对合法值。
	Status string `gorm:"column:status;->"`
	// PublishedObjectID 为 nil 时表示尚未关联实际发布对象。
	PublishedObjectID *uint64 `gorm:"column:published_object_id;->"`
	// ResolvedName 为 nil 时表示尚未确定最终发布名称。
	ResolvedName *string `gorm:"column:resolved_name;->"`
	// ResultEntryID 为 nil 时表示尚未关联发布结果目录项。
	ResultEntryID *uint64 `gorm:"column:result_entry_id;->"`
	// ReservedBytes 是本任务当前仍持有的预占字节数。
	ReservedBytes uint64 `gorm:"column:reserved_bytes;->"`

	// 4. 映射租约、重试和生命周期事实。
	// LeaseOwner 为 nil 时表示没有执行租约持有者。
	LeaseOwner *string `gorm:"column:lease_owner;->"`
	// LeaseUntil 是可空的执行租约截止 UTC 时间。
	LeaseUntil *time.Time `gorm:"column:lease_until;->"`
	// LeaseVersion 是领取租约时递增的完整无符号栅栏版本。
	LeaseVersion uint64 `gorm:"column:lease_version;->"`
	// NextAttemptAt 是可空的下次恢复执行 UTC 时间。
	NextAttemptAt *time.Time `gorm:"column:next_attempt_at;->"`
	// AttemptCount 是已记录的恢复次数，对齐 INT UNSIGNED。
	AttemptCount uint32 `gorm:"column:attempt_count;->"`
	// ExpiresAt 是任务到期 UTC 时间，由后续服务计算并写入。
	ExpiresAt time.Time `gorm:"column:expires_at;->"`
	// LastErrorCode 是可空的机器错误码。
	LastErrorCode *string `gorm:"column:last_error_code;->"`
	// LastErrorMessage 是可空的脱敏诊断摘要。
	LastErrorMessage *string `gorm:"column:last_error_message;->"`
	// CreatedAt 是任务创建 UTC 时间。
	CreatedAt time.Time `gorm:"column:created_at;->"`
	// UpdatedAt 是任务记录最近更新 UTC 时间。
	UpdatedAt time.Time `gorm:"column:updated_at;->"`
	// CompletedAt 是可空的任务终结 UTC 时间，覆盖成功、取消、过期及失败。
	CompletedAt *time.Time `gorm:"column:completed_at;->"`
}

// TableName 指向既有上传任务表。
func (UploadTask) TableName() string {
	// 1. 返回既有上传任务表名。
	return "upload_tasks"
}
