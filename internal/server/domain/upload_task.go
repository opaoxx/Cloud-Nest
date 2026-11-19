package domain

import (
	"crypto/sha256"
	"errors"
	"time"
)

// ErrUploadTaskNotFound 表示在指定用户范围内未找到上传任务。
var ErrUploadTaskNotFound = errors.New("上传任务不存在")

// ErrInvalidUploadTaskData 表示上传任务的归属、声明或状态事实异常。
var ErrInvalidUploadTaskData = errors.New("上传任务数据异常")

// ErrUploadTaskStagedObjectConflict 表示暂存对象已经关联其他上传任务。
var ErrUploadTaskStagedObjectConflict = errors.New("暂存对象已关联上传任务")

// ErrUploadTaskNotReady 表示任务方式、阶段或期限不满足当前内容接收条件。
var ErrUploadTaskNotReady = errors.New("上传任务当前不可接收文件内容")

// ErrUploadTargetUnavailable 表示目标路径当前无法容纳发布的文件。
var ErrUploadTargetUnavailable = errors.New("目标目录已不可用")

// UploadTargetUnavailableCode 标记目标目录失效导致的任务失败。
const UploadTargetUnavailableCode = "TARGET_UNAVAILABLE"

// ErrUploadTaskBusy 表示当前任务仍由有效租约持有者处理。
var ErrUploadTaskBusy = errors.New("上传任务正在处理中")

// ErrUploadExpirationRetryable 表示过期收尾遇到已确认的临时数据库错误。
var ErrUploadExpirationRetryable = errors.New("上传过期收尾可重试")

// ErrUploadCancellationRetryable 表示取消收尾遇到已确认的临时数据库错误。
var ErrUploadCancellationRetryable = errors.New("上传取消收尾可重试")

// ErrUploadLeaseVersionExhausted 表示租约版本已达 uint64 上限，无法继续递增。
var ErrUploadLeaseVersionExhausted = errors.New("上传租约版本已达上限")

// UploadTask 保存上传任务的内部记录，供后续持久化、发布和恢复流程使用。
type UploadTask struct {
	// 1. 保存任务标识、归属及当前暂存对象。
	// ID 对应上传任务的完整无符号主键。
	ID uint64
	// UserID 是任务所属用户，由可信身份确定。
	UserID uint64
	// DeviceID 是创建任务时的来源设备。
	DeviceID uint64
	// StagedObjectID 是当前上传尝试使用的暂存对象。
	StagedObjectID uint64

	// 2. 保存目标目录、上传声明及分片信息。
	// TargetParentID 是任务准备发布到的目标目录。
	TargetParentID uint64
	// RequestedName 是通过统一命名校验的请求展示名。
	RequestedName string
	// UploadMode 保存服务端确定的 SINGLE 或 MULTIPART。
	UploadMode string
	// StorageUploadID 为 nil 时表示没有对象存储分片会话。
	StorageUploadID *string
	// ExpectedSHA256 为 nil 时表示未声明预期摘要，非空时保存完整 32 字节。
	ExpectedSHA256 *[sha256.Size]byte
	// FileSize 是声明的完整无符号字节数。
	FileSize uint64
	// PartSize 为 nil 时表示没有分片大小规划。
	PartSize *uint64
	// PartCount 为 nil 时表示没有分片数量规划，对齐 INT UNSIGNED。
	PartCount *uint32

	// 3. 保存任务状态、发布结果及剩余预占。
	// Status 是数据库保存的上传阶段，复用精确状态类型。
	Status UploadStatus
	// PublishedObjectID 为 nil 时表示尚未关联实际发布对象。
	PublishedObjectID *uint64
	// ResolvedName 为 nil 时表示尚未确定最终发布名称。
	ResolvedName *string
	// ResultEntryID 为 nil 时表示尚未关联发布结果目录项。
	ResultEntryID *uint64
	// ReservedBytes 是本任务当前仍持有的预占字节数。
	ReservedBytes uint64

	// 4. 保存租约、重试和生命周期事实。
	// LeaseOwner 为 nil 时表示没有执行租约持有者。
	LeaseOwner *string
	// LeaseUntil 是可空的执行租约截止 UTC 时间。
	LeaseUntil *time.Time
	// LeaseVersion 是领取租约时递增的完整无符号栅栏版本。
	LeaseVersion uint64
	// NextAttemptAt 是可空的下次恢复执行 UTC 时间。
	NextAttemptAt *time.Time
	// AttemptCount 是已记录的恢复次数，对齐 INT UNSIGNED。
	AttemptCount uint32
	// ExpiresAt 是任务到期 UTC 时间，由后续服务计算并写入。
	ExpiresAt time.Time
	// LastErrorCode 是可空的机器错误码。
	LastErrorCode *string
	// LastErrorMessage 是可空的脱敏诊断摘要。
	LastErrorMessage *string
	// CreatedAt 是任务创建 UTC 时间。
	CreatedAt time.Time
	// UpdatedAt 是任务记录最近更新 UTC 时间。
	UpdatedAt time.Time
	// CompletedAt 是可空的任务终结 UTC 时间，覆盖成功、取消、过期及失败。
	CompletedAt *time.Time
}
