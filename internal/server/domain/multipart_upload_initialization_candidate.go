package domain

// MaxMultipartUploadInitializationCandidates 限定一轮初始化扫描的数量。
const MaxMultipartUploadInitializationCandidates = 100

// MultipartUploadInitializationCandidate 保存初始化租约领取前读取的任务定位。
type MultipartUploadInitializationCandidate struct {
	// TaskID 和 UserID 用于按任务所属账号重新读取记录。
	TaskID uint64
	UserID uint64
	// StagedObjectID 和 LeaseVersion 供领取事务复查候选是否仍有效。
	StagedObjectID uint64
	LeaseVersion   uint64
}
