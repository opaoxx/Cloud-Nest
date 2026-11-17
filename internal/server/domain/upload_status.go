package domain

// UploadStatus 保存 upload_tasks.status 的精确状态值。
type UploadStatus string

const (
	// UploadStatusCreating 表示正在建立上传任务及存储准备信息。
	UploadStatusCreating UploadStatus = "CREATING"
	// UploadStatusUploading 表示任务等待或正在接收文件内容。
	UploadStatusUploading UploadStatus = "UPLOADING"
	// UploadStatusCompleting 表示正在完成对象存储的分片合并。
	UploadStatusCompleting UploadStatus = "COMPLETING"
	// UploadStatusVerifying 表示内容已提交，等待或正在校验与发布。
	UploadStatusVerifying UploadStatus = "VERIFYING"
	// UploadStatusCompleted 表示文件已成功发布，任务终结。
	UploadStatusCompleted UploadStatus = "COMPLETED"
	// UploadStatusCancelling 表示正在处理取消及预占释放。
	UploadStatusCancelling UploadStatus = "CANCELLING"
	// UploadStatusCancelled 表示取消已完成，任务终结。
	UploadStatusCancelled UploadStatus = "CANCELLED"
	// UploadStatusExpiring 表示正在处理过期及预占释放。
	UploadStatusExpiring UploadStatus = "EXPIRING"
	// UploadStatusExpired 表示过期处理已完成，任务终结。
	UploadStatusExpired UploadStatus = "EXPIRED"
	// UploadStatusFailed 表示任务因不可恢复错误终结。
	UploadStatusFailed UploadStatus = "FAILED"
)

// IsValid 检查状态是否属于数据库允许的上传阶段。
func (status UploadStatus) IsValid() bool {
	// 1. 按数据库约束精确匹配十种合法状态。
	switch status {
	case UploadStatusCreating, UploadStatusUploading, UploadStatusCompleting,
		UploadStatusVerifying, UploadStatusCompleted, UploadStatusCancelling,
		UploadStatusCancelled, UploadStatusExpiring, UploadStatusExpired, UploadStatusFailed:
		return true
	}
	// 2. 其余值返回 false。
	return false
}

// IsTerminal 判断上传任务是否已经终结。
func (status UploadStatus) IsTerminal() bool {
	// 1. 识别已完成、已取消、已过期和失败四种终态。
	switch status {
	case UploadStatusCompleted, UploadStatusCancelled, UploadStatusExpired, UploadStatusFailed:
		return true
	}
	// 2. 其余值返回 false。
	return false
}
