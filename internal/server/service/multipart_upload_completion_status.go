package service

import (
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ResolveMultipartUploadCompletionStatus 判定已按归属读取的 Multipart 完成请求应进入的分支。
// UPLOADING 要提交回执。COMPLETING、VERIFYING 和 COMPLETED 可复用当前状态及结果。
// databaseTime 只用于判定能否新受理。决定写入前，调用方仍须在事务中复核当前任务。
func ResolveMultipartUploadCompletionStatus(task *domain.UploadTask, userID, taskID uint64, databaseTime time.Time) (domain.UploadStatus, error) {
	// 1. 核对任务归属、上传模式和阶段。
	if task == nil || userID == 0 || taskID == 0 || task.ID != taskID || task.UserID != userID ||
		task.DeviceID == 0 || task.StagedObjectID == 0 || task.TargetParentID == 0 || !task.Status.IsValid() {
		return "", domain.ErrInvalidUploadTaskData
	}
	switch task.UploadMode {
	case "MULTIPART":
	case "SINGLE":
		return "", domain.ErrUploadTaskNotReady
	default:
		return "", domain.ErrInvalidUploadTaskData
	}

	// 2. 对 UPLOADING 检查是否可以开始新一轮回执核对。
	if task.Status == domain.UploadStatusUploading {
		if err := validateUploadPartStateWithoutActiveLease(task, userID, taskID, databaseTime); err != nil {
			return "", err
		}
		return task.Status, nil
	}

	// 3. 对已受理阶段核对已保存的进度或结果，其余阶段拒绝。
	switch task.Status {
	case domain.UploadStatusCompleting, domain.UploadStatusVerifying:
		if task.PublishedObjectID != nil || task.ResultEntryID != nil || task.CompletedAt != nil {
			return "", domain.ErrInvalidUploadTaskData
		}
	case domain.UploadStatusCompleted:
		if task.PublishedObjectID == nil || *task.PublishedObjectID == 0 || task.ResultEntryID == nil || *task.ResultEntryID == 0 ||
			task.CompletedAt == nil || !validUploadDatabaseTime(*task.CompletedAt) || task.ReservedBytes != 0 {
			return "", domain.ErrInvalidUploadTaskData
		}
	default:
		return "", domain.ErrUploadTaskNotReady
	}
	return task.Status, nil
}
