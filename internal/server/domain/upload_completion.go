package domain

import "errors"

// ErrUploadPartsMismatch 表示当前分片不足以受理完成请求。
var ErrUploadPartsMismatch = errors.New("分片回执不一致，请核对后补传")

// UploadPartsMismatchError 保存升序的受影响编号，最多交付前 20 个。
type UploadPartsMismatchError struct {
	PartNumbers []uint32
}

func (*UploadPartsMismatchError) Error() string { return ErrUploadPartsMismatch.Error() }

func (*UploadPartsMismatchError) Unwrap() error { return ErrUploadPartsMismatch }

// UploadPartCompletionClaim 保存客户端提交的分片回执声明，受理完成前须与存储核对。
type UploadPartCompletionClaim struct {
	PartNumber uint32
	ETag       string
}

// ResolveSingleUploadCompletionStatus 判定已按归属读取的单文件任务完成状态。
// 结果为 VERIFYING 或 COMPLETED。租约、重试和到期由后台流程推进。
func ResolveSingleUploadCompletionStatus(task *UploadTask) (UploadStatus, error) {
	// 1. 校验快照的上传模式和任务阶段。
	if task == nil || !task.Status.IsValid() {
		return "", ErrInvalidUploadTaskData
	}
	switch task.UploadMode {
	case "SINGLE":
		return ResolveUploadCompletionResultStatus(task)
	case "MULTIPART":
		return "", ErrUploadTaskNotReady
	default:
		return "", ErrInvalidUploadTaskData
	}
}

// ResolveUploadCompletionResultStatus 只判定已受理的任务进度或发布结果。
func ResolveUploadCompletionResultStatus(task *UploadTask) (UploadStatus, error) {
	// 1. 核对上传模式与阶段是否允许交付成功结果。
	if task == nil || !task.Status.IsValid() || (task.UploadMode != "SINGLE" && task.UploadMode != "MULTIPART") {
		return "", ErrInvalidUploadTaskData
	}
	if task.Status == UploadStatusCompleting && task.UploadMode != "MULTIPART" {
		return "", ErrUploadTaskNotReady
	}

	// 2. 按当前阶段核对是否已有发布事实。
	switch task.Status {
	case UploadStatusCompleting, UploadStatusVerifying:
		if task.PublishedObjectID != nil || task.ResultEntryID != nil || task.CompletedAt != nil {
			return "", ErrInvalidUploadTaskData
		}
	case UploadStatusCompleted:
		if task.PublishedObjectID == nil || *task.PublishedObjectID == 0 || task.ResultEntryID == nil || *task.ResultEntryID == 0 ||
			task.CompletedAt == nil || task.CompletedAt.IsZero() || task.ReservedBytes != 0 {
			return "", ErrInvalidUploadTaskData
		}
	default:
		return "", ErrUploadTaskNotReady
	}
	return task.Status, nil
}
