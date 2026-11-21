package service

import (
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ValidateUploadPartSigningState 判断按归属读取的当前任务是否满足分片签发的阶段条件。
// databaseTime 由调用方从数据库取得。任务和租约分别在到达自身截止时刻时失效。
func ValidateUploadPartSigningState(task *domain.UploadTask, userID, taskID uint64, databaseTime time.Time) error {
	// 1. 复查任务和账号匹配，并核对数据库时间及记录状态。
	if task == nil || userID == 0 || taskID == 0 || task.ID != taskID || task.UserID != userID ||
		task.DeviceID == 0 || task.StagedObjectID == 0 || task.TargetParentID == 0 ||
		!task.Status.IsValid() || (task.UploadMode != "SINGLE" && task.UploadMode != "MULTIPART") ||
		!validUploadDatabaseTime(databaseTime) || !validUploadDatabaseTime(task.ExpiresAt) {
		return domain.ErrInvalidUploadTaskData
	}

	// 2. 要求未到期的分片任务、已绑定会话和完整的上传声明。
	if task.UploadMode != "MULTIPART" || task.Status != domain.UploadStatusUploading || !databaseTime.Before(task.ExpiresAt) ||
		task.StorageUploadID == nil || !domain.ValidMultipartUploadID(*task.StorageUploadID) {
		return domain.ErrUploadTaskNotReady
	}
	if task.PartSize == nil || task.PartCount == nil || !domain.ValidMultipartUploadLayout(task.FileSize, *task.PartSize, *task.PartCount) ||
		task.ExpectedSHA256 == nil || task.ReservedBytes != task.FileSize || task.PublishedObjectID != nil ||
		task.ResolvedName != nil || task.ResultEntryID != nil || task.CompletedAt != nil {
		return domain.ErrInvalidUploadTaskData
	}

	// 3. 正在进行的执行租约会阻止签发。已到期的租约保留在记录中也不阻止。
	if err := validateUploadLeaseData(task); err != nil {
		return err
	}
	if task.LeaseUntil != nil && databaseTime.Before(*task.LeaseUntil) {
		return domain.ErrUploadTaskBusy
	}
	return nil
}
