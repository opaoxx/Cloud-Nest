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
