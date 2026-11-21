package domain

// MaxMultipartUploadCompletionCandidates 限定一轮后台合并扫描的数量。
const MaxMultipartUploadCompletionCandidates = 100

// MultipartUploadCompletionCandidate 保存后台合并领取租约前读取的任务定位。
type MultipartUploadCompletionCandidate struct {
	// TaskID 和 UserID 用于在所属用户事务中重新定位任务。
	TaskID uint64
	UserID uint64
	// StagedObjectID 和 LeaseVersion 供重新加锁后检查候选是否仍然有效。
	StagedObjectID uint64
	LeaseVersion   uint64
}
