package models

import "time"

// NewUploadTask 映射 SINGLE 和 MULTIPART 上传任务创建字段。
// 任务标识通过暂存对象唯一键回读，其余列采用数据库初始值。
type NewUploadTask struct {
	// 1. 映射可信归属、来源设备及暂存对象引用。
	// UserID 来自已认证用户事务绑定的身份。
	UserID uint64 `gorm:"column:user_id;<-:create"`
	// DeviceID 来自已认证事务中的来源设备。
	DeviceID uint64 `gorm:"column:device_id;<-:create"`
	// StagedObjectID 引用同一用户的本次暂存对象，须由创建流程核对。
	StagedObjectID uint64 `gorm:"column:staged_object_id;<-:create"`

	// 2. 映射目标目录、文件声明和上传方式。
	// TargetParentID 是已经过归属及路径检查的目标目录。
	TargetParentID uint64 `gorm:"column:target_parent_id;<-:create"`
	// RequestedName 是通过统一命名校验的请求展示名。
	RequestedName string `gorm:"column:requested_name;<-:create"`
	// UploadMode 由任务规划确定为 SINGLE 或 MULTIPART。
	UploadMode string `gorm:"column:upload_mode;<-:create"`
	// ExpectedSHA256 承接可空的完整 32 字节预期摘要，nil 写入 SQL NULL。
	ExpectedSHA256 []byte `gorm:"column:expected_sha256;<-:create"`
	// FileSize 是完整无符号声明字节数，上传能力上限由上层校验。
	FileSize uint64 `gorm:"column:file_size;<-:create"`
	// PartSize 和 PartCount 保存 Multipart 的固定计划，SINGLE 由数据库保留 NULL。
	PartSize  *uint64 `gorm:"column:part_size;<-:create"`
	PartCount *uint32 `gorm:"column:part_count;<-:create"`

	// 3. 映射初始阶段、预占字节和到期时间。
	// Status 是 SINGLE 的 UPLOADING 或 Multipart 的 CREATING。
	Status string `gorm:"column:status;<-:create"`
	// ReservedBytes 是本任务持有的预占字节，创建时等于完整声明大小。
	ReservedBytes uint64 `gorm:"column:reserved_bytes;<-:create"`
	// ExpiresAt 是创建流程确定的 UTC 微秒到期时间。
	ExpiresAt time.Time `gorm:"column:expires_at;<-:create"`
}

// TableName 指向既有上传任务表。
func (NewUploadTask) TableName() string {
	// 1. 返回既有上传任务表名。
	return "upload_tasks"
}

// NewUploadReplacementTask 在基础创建字段上增加两个必填的替换绑定列。
// 嵌入的 NewUploadTask 提供共用列和 upload_tasks 表名。
type NewUploadReplacementTask struct {
	NewUploadTask
	// ReplacementEntryID 指向同账号的目标文件。
	ReplacementEntryID uint64 `gorm:"column:replacement_entry_id;<-:create"`
	// ReplacementExpectedRevision 保存准备时锁读成功的预期版本。
	ReplacementExpectedRevision uint64 `gorm:"column:replacement_expected_revision;<-:create"`
}
