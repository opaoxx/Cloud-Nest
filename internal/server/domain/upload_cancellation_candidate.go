package domain

// UploadCancellationCandidate 保存取消收尾前扫描到的任务定位。
type UploadCancellationCandidate struct {
	// TaskID 和 UserID 用于按所属用户重新读取任务。
	TaskID uint64
	UserID uint64
	// StagedObjectID 和 LeaseVersion 用于复核扫描后任务是否仍是同一次处理。
	StagedObjectID uint64
	LeaseVersion   uint64
}
