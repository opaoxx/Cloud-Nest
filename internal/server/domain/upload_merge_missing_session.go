package domain

import "errors"

// UploadSessionLostCode 标记已确认原会话与合并对象均不存在的失败终态。
const UploadSessionLostCode = "UPLOAD_SESSION_LOST"

// ErrUploadSessionLost 提供会话丢失终态的固定提示。
var ErrUploadSessionLost = errors.New("存储上传会话已失效，请重新创建任务")

// UploadMergeMissingSessionObservation 记录同一租约下原会话和对象均未查到。
// 失败写回前，调用方还须确认这次观察发生在存储合并结束之后。
type UploadMergeMissingSessionObservation struct {
	Task      UploadTask
	ObjectKey string
}
