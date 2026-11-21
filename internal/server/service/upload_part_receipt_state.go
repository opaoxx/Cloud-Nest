package service

import (
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ValidateUploadPartReceiptState 判断按归属读取的任务能否只读核对分片回执。
// databaseTime 来自读取任务时的数据库时钟。有效租约期间仍可核对 UPLOADING 任务。
func ValidateUploadPartReceiptState(task *domain.UploadTask, userID, taskID uint64, databaseTime time.Time) error {
	// 1. 复查任务和账号匹配，并核对数据库时间及记录状态。
	if task == nil || userID == 0 || taskID == 0 || task.ID != taskID || task.UserID != userID ||
		task.DeviceID == 0 || task.StagedObjectID == 0 || task.TargetParentID == 0 ||
		!task.Status.IsValid() || (task.UploadMode != "SINGLE" && task.UploadMode != "MULTIPART") ||
		!validUploadDatabaseTime(databaseTime) || !validUploadDatabaseTime(task.ExpiresAt) {
		return domain.ErrInvalidUploadTaskData
	}

	// 2. 确认可读阶段、有效期限和已绑定的存储会话。
	if task.UploadMode != "MULTIPART" || task.Status != domain.UploadStatusUploading || !databaseTime.Before(task.ExpiresAt) ||
		task.StorageUploadID == nil || !domain.ValidMultipartUploadID(*task.StorageUploadID) {
		return domain.ErrUploadTaskNotReady
	}

	// 3. 核对保存的分片计划、上传声明和租约字段。
	if task.PartSize == nil || task.PartCount == nil || !domain.ValidMultipartUploadLayout(task.FileSize, *task.PartSize, *task.PartCount) ||
		task.ExpectedSHA256 == nil || task.ReservedBytes != task.FileSize || task.PublishedObjectID != nil ||
		task.ResolvedName != nil || task.ResultEntryID != nil || task.CompletedAt != nil {
		return domain.ErrInvalidUploadTaskData
	}
	return validateUploadLeaseData(task)
}
