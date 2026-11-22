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
