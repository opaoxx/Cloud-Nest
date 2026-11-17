package service

import (
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ValidateUploadContentState 检查已读取任务是否满足首期内容接收的阶段条件。
// 调用方在已认证用户事务中读取当前记录，后续租约领取继续核对接收权。
// now 由调用方传入，任务在 now >= expires_at 时视为到期，保留输入原值。
func ValidateUploadContentState(task *domain.UploadTask, userID, taskID uint64, now time.Time) error {
	// 1. 检查用户、任务标识和判断时间。
	if userID == 0 || taskID == 0 || now.IsZero() {
		return fmt.Errorf("校验上传接收状态需要非零用户和任务 ID 及判断时间")
	}

	// 2. 核对记录归属、基础标识及状态数据。
	if task == nil || task.ID != taskID || task.UserID != userID || task.DeviceID == 0 || task.StagedObjectID == 0 || task.TargetParentID == 0 ||
		!task.Status.IsValid() || (task.UploadMode != "SINGLE" && task.UploadMode != "MULTIPART") {
		return domain.ErrInvalidUploadTaskData
	}
	expiry := task.ExpiresAt.UTC()
	if expiry.Year() < 1000 || expiry.Year() > 9999 {
		return domain.ErrInvalidUploadTaskData
	}

	// 3. 检查 SINGLE、UPLOADING 和任务期限。
	if task.UploadMode != "SINGLE" || task.Status != domain.UploadStatusUploading || !now.Before(expiry) {
		return domain.ErrUploadTaskNotReady
	}

	// 4. 核对声明大小、完整预占及接收阶段字段。
	if task.FileSize > domain.MaxSingleUploadSizeBytes {
		return domain.ErrUploadFileTooLarge
	}
	if task.ReservedBytes != task.FileSize || task.StorageUploadID != nil || task.PartSize != nil || task.PartCount != nil ||
		task.PublishedObjectID != nil || task.ResolvedName != nil || task.ResultEntryID != nil || task.CompletedAt != nil {
		return domain.ErrInvalidUploadTaskData
	}
	return nil
}
