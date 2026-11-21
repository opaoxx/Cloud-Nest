package service

import (
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ValidateUploadPartSigningState 判断按归属读取的当前任务是否满足分片签发的阶段条件。
// databaseTime 由调用方从数据库取得。任务和租约分别在到达自身截止时刻时失效。
func ValidateUploadPartSigningState(task *domain.UploadTask, userID, taskID uint64, databaseTime time.Time) error {
	return validateUploadPartStateWithoutActiveLease(task, userID, taskID, databaseTime)
}

// validateUploadPartStateWithoutActiveLease 是分片签发和首次完成核对共用的进入条件。
func validateUploadPartStateWithoutActiveLease(task *domain.UploadTask, userID, taskID uint64, databaseTime time.Time) error {
	// 1. 复用分片任务的归属、阶段、期限、会话和记录校验。
	if err := ValidateUploadPartReceiptState(task, userID, taskID, databaseTime); err != nil {
		return err
	}

	// 2. 有效执行租约阻止发起操作，记录中的已到期租约允许后续领取。
	if task.LeaseUntil != nil && databaseTime.Before(*task.LeaseUntil) {
		return domain.ErrUploadTaskBusy
	}
	return nil
}
