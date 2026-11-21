package domain

// MaxUploadCancellationCandidates 是每轮可处理的取消候选上限。
const MaxUploadCancellationCandidates = 100

// UploadCancellationCandidate 保存取消收尾前扫描到的任务定位。
type UploadCancellationCandidate struct {
	// TaskID 和 UserID 用于按所属用户重新读取任务。
	TaskID uint64
	UserID uint64
	// StagedObjectID 和 LeaseVersion 用于复核扫描后任务是否仍是同一次处理。
	StagedObjectID uint64
	LeaseVersion   uint64
}

// MultipartUploadCancellationTarget 保存当前取消租约所绑定的存储目标。
type MultipartUploadCancellationTarget struct {
	// ObjectKey 是暂存对象的原始不可变键。
	ObjectKey string
	// UploadID 原样保存存储会话 ID。nil 表示任务尚未绑定会话。
	UploadID *string
}

// MultipartUploadCancellationObservation 保存与任务、租约及原存储绑定匹配的观察。
// Object 为 nil 表示原对象明确缺失；若 Task 已绑定会话，也已确认该会话缺失。
// Task 尚未绑定时，可能存在未关联会话，由独立的孤儿会话流程处理。
type MultipartUploadCancellationObservation struct {
	Task      UploadTask
	ObjectKey string
	Object    *UploadObjectResult
}

// MultipartUploadCancellationRetryReason 指定取消任务再次尝试的原因。
type MultipartUploadCancellationRetryReason string

const (
	// MultipartCancellationRetryStorage 表示存储状态暂时无法确认。
	MultipartCancellationRetryStorage MultipartUploadCancellationRetryReason = "STORAGE_UNAVAILABLE"
	// MultipartCancellationRetryDatabase 表示收尾事务遇到已确认的临时数据库故障。
	MultipartCancellationRetryDatabase MultipartUploadCancellationRetryReason = "DATABASE_UNAVAILABLE"
)
