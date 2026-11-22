package domain

// MultipartUploadSessionScanTarget 定位待查询会话的原暂存对象及其任务。
type MultipartUploadSessionScanTarget struct {
	ObjectID  uint64
	UserID    uint64
	TaskID    uint64
	ObjectKey string
}

// MultipartUploadSessionScanPage 保存一页对象键和后续读取位置。
type MultipartUploadSessionScanPage struct {
	Targets []MultipartUploadSessionScanTarget
	// NextAfterObjectID 为零表示末页。非零时即使 Targets 为空也应继续。
	NextAfterObjectID uint64
}

// MultipartUploadSessionProtection 是单条存储会话在数据库中的当前保护事实。
type MultipartUploadSessionProtection struct {
	// Bound 表示存储会话与任务中保存的会话 ID 完全相同。
	Bound bool
	// ActiveCreationLease 表示原任务仍在持有效租约创建会话。
	ActiveCreationLease bool
}
