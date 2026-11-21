package service

import (
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ValidateUploadContentLease 检查当前任务是否满足接收租约的领取条件。
// 调用方在已认证事务内锁定任务，并传入判断时间，随后在同一事务中条件写入。
// 本方法保留输入原值，now >= lease_until 时租约视为到期。
func ValidateUploadContentLease(task *domain.UploadTask, userID, taskID uint64, now time.Time) error {
	// 1. 复用上传内容接收阶段校验。
	if err := ValidateUploadContentState(task, userID, taskID, now); err != nil {
		return err
	}

	// 2. 核对租约持有者、期限配对及已领取版本。
	if err := validateUploadLeaseData(task); err != nil {
		return err
	}

	// 3. 有效租约返回忙碌，阻止重复领取。
	if task.LeaseUntil != nil && now.Before(*task.LeaseUntil) {
		return domain.ErrUploadTaskBusy
	}

	// 4. 检查版本递增上限，返回可继续领取的结果。
	if task.LeaseVersion == ^uint64(0) {
		return domain.ErrUploadLeaseVersionExhausted
	}
	return nil
}

// validateUploadLeaseData 检查读取的任务是否保留完整、有效的租约字段。
func validateUploadLeaseData(task *domain.UploadTask) error {
	if (task.LeaseOwner == nil) != (task.LeaseUntil == nil) {
		return domain.ErrInvalidUploadTaskData
	}
	if task.LeaseOwner != nil {
		if len(*task.LeaseOwner) == 0 || len(*task.LeaseOwner) > 64 || task.LeaseVersion == 0 {
			return domain.ErrInvalidUploadTaskData
		}
		for _, character := range *task.LeaseOwner {
			if character > 127 {
				return domain.ErrInvalidUploadTaskData
			}
		}
		if !validUploadDatabaseTime(*task.LeaseUntil) {
			return domain.ErrInvalidUploadTaskData
		}
	}
	return nil
}

func validUploadDatabaseTime(stamp time.Time) bool {
	year := stamp.UTC().Year()
	return year >= 1000 && year <= 9999 && stamp.Nanosecond()%1000 == 0
}
