package domain

import "time"

// MaxUploadExpirationCandidates 是每轮可扫描的到期候选上限。
const MaxUploadExpirationCandidates = 100

// UploadExpirationCandidate 是上传过期处理使用的只读定位快照。
type UploadExpirationCandidate struct {
	// TaskID 与 UserID 用于在所属用户事务中定位任务。
	TaskID uint64
	UserID uint64
	// StagedObjectID 与 LeaseVersion 用于复核扫描后是否切换了尝试。
	StagedObjectID uint64
	LeaseVersion   uint64
	// Status 和 ExpiresAt 记录扫描时的阶段和到期时间。
	Status    UploadStatus
	ExpiresAt time.Time
	// UploadMode 区分单文件和分片清理入口。
	UploadMode string
}
