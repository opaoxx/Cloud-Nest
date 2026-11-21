package service

import (
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ResolveUploadPartRenewalExpiry 判定一次显式续期并计算截止时间。
// 调用方须在已认证的账号事务中提供当前任务、同账号配额及数据库时间。
func ResolveUploadPartRenewalExpiry(task *domain.UploadTask, account *domain.UserProfile, expectedExpiry, databaseTime time.Time) (time.Time, error) {
	// 1. 核对活动账号、任务资格及客户端上次看到的截止时间。
	if account == nil || account.ID == 0 {
		return time.Time{}, domain.ErrInvalidAccountData
	}
	switch account.Status {
	case "ACTIVE":
	case "DISABLED":
		return time.Time{}, domain.ErrAccountDisabled
	default:
		return time.Time{}, domain.ErrInvalidAccountData
	}
	if task == nil {
		return time.Time{}, domain.ErrInvalidUploadTaskData
	}
	if err := ValidateUploadPartSigningState(task, account.ID, task.ID, databaseTime); err != nil {
		return time.Time{}, err
	}
	if !expectedExpiry.Equal(task.ExpiresAt) {
		return time.Time{}, domain.ErrUploadTaskNotReady
	}

	// 2. 确认账号总预占仍覆盖本任务，先比较已用空间再做减法，避免溢出。
	if account.ReservedBytes < task.ReservedBytes || account.UsedBytes > account.QuotaBytes ||
		account.ReservedBytes > account.QuotaBytes-account.UsedBytes {
		return time.Time{}, domain.ErrUploadTaskNotReady
	}

	// 3. 用数据库时间计算新期限，只延长时才采用新值。
	renewedExpiry := databaseTime.UTC().Add(uploadTaskLifetime)
	if !validUploadDatabaseTime(renewedExpiry) {
		return time.Time{}, domain.ErrInvalidUploadTaskData
	}
	if renewedExpiry.After(task.ExpiresAt) {
		return renewedExpiry, nil
	}
	return task.ExpiresAt.UTC(), nil
}
