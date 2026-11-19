package domain

// UploadVerificationCandidate 保存校验 worker 在领取租约前看到的内部任务定位。
type UploadVerificationCandidate struct {
	// TaskID 和 UserID 用于定位当前用户的任务并重新加锁。
	TaskID uint64
	UserID uint64
	// StagedObjectID 是扫描时任务引用的暂存对象。
	StagedObjectID uint64
	// LeaseVersion 是领取前的预期版本，领取仍须重新核对当前版本。
	LeaseVersion uint64
}
