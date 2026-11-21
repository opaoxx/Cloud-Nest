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
	case "MULTIPART":
		return "", ErrUploadTaskNotReady
	default:
		return "", ErrInvalidUploadTaskData
	}

	// 2. 核对当前阶段的发布事实，再交付校验中或已完成状态。
	switch task.Status {
	case UploadStatusVerifying:
		if task.PublishedObjectID != nil || task.ResultEntryID != nil || task.CompletedAt != nil {
			return "", ErrInvalidUploadTaskData
		}
		return UploadStatusVerifying, nil
	case UploadStatusCompleted:
		if task.PublishedObjectID == nil || *task.PublishedObjectID == 0 || task.ResultEntryID == nil || *task.ResultEntryID == 0 ||
			task.CompletedAt == nil || task.CompletedAt.IsZero() || task.ReservedBytes != 0 {
			return "", ErrInvalidUploadTaskData
		}
		return UploadStatusCompleted, nil
	default:
		// 3. 其他已知阶段由任务查询交付实际状态。
		return "", ErrUploadTaskNotReady
	}
}
