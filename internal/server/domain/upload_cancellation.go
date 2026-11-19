package domain

import "time"

// ShouldBeginUploadCancellation 判断已按归属读取的任务是否需要进入取消阶段。
// true 表示可写入 CANCELLING。false, nil 表示可直接交付当前状态。
// 调用方在所属用户事务中提供锁定的当前记录和数据库时间。
func ShouldBeginUploadCancellation(task *UploadTask, now time.Time) (bool, error) {
	// 1. 检查任务状态和上传方式。
	if task == nil || now.IsZero() || !task.Status.IsValid() {
		return false, ErrInvalidUploadTaskData
	}
	switch task.UploadMode {
	case "SINGLE":
	case "MULTIPART":
		return false, ErrUploadTaskNotReady
	default:
		return false, ErrInvalidUploadTaskData
	}

	// 2. 已在取消中或已到终态时，复用当前状态。
	if task.Status == UploadStatusCancelling || task.Status.IsTerminal() {
		return false, nil
	}

	// 3. 对可取消的在途阶段，先判断任务期限，再判断租约。
	switch task.Status {
	case UploadStatusCreating, UploadStatusUploading, UploadStatusCompleting, UploadStatusVerifying:
	default:
		return false, ErrUploadTaskNotReady
	}
	if task.ExpiresAt.IsZero() {
		return false, ErrInvalidUploadTaskData
	}
	if !now.Before(task.ExpiresAt) {
		return false, ErrUploadTaskNotReady
	}
	if (task.LeaseOwner == nil) != (task.LeaseUntil == nil) {
		return false, ErrInvalidUploadTaskData
	}
	if task.LeaseUntil != nil {
		if task.LeaseUntil.IsZero() {
			return false, ErrInvalidUploadTaskData
		}
		if now.Before(*task.LeaseUntil) {
			return false, ErrUploadTaskBusy
		}
	}
	return true, nil
}
