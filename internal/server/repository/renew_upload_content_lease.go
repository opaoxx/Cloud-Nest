package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// RenewUserUploadContentLease 在已认证用户事务中延长当前内容接收租约。
// 调用方在外层事务提交成功后使用返回的期限。
func (store *Store) RenewUserUploadContentLease(ctx context.Context, taskID, objectID, leaseVersion uint64, leaseOwner string) (*domain.UploadTask, error) {
	// 1. 校验续租参数，加锁读取当前用户的任务。
	if store == nil || ctx == nil || taskID == 0 || objectID == 0 {
		return nil, fmt.Errorf("续租上传接收需要上下文、非零任务及对象 ID 和已认证用户事务")
	}
	if leaseVersion == 0 || len(leaseOwner) == 0 || len(leaseOwner) > 64 {
		return nil, domain.ErrInvalidUploadTaskData
	}
	for _, character := range leaseOwner {
		if character > 127 {
			return nil, domain.ErrInvalidUploadTaskData
		}
	}
	renewContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	task, err := store.LockUserUploadTask(renewContext, taskID)
	if err != nil {
		return nil, err
	}

	// 2. 核对接收阶段、暂存对象、持有者和版本。
	if task.UploadMode != "SINGLE" || task.Status != domain.UploadStatusUploading {
		return nil, domain.ErrUploadTaskNotReady
	}
	if task.FileSize > domain.MaxSingleUploadSizeBytes {
		return nil, domain.ErrUploadFileTooLarge
	}
	if task.ReservedBytes != task.FileSize || task.PublishedObjectID != nil || task.ResolvedName != nil ||
		task.ResultEntryID != nil || task.CompletedAt != nil {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if task.StagedObjectID != objectID || task.LeaseVersion != leaseVersion || task.LeaseOwner == nil ||
		task.LeaseUntil == nil || *task.LeaseOwner != leaseOwner {
		return nil, domain.ErrUploadTaskBusy
	}

	// 3. 用数据库时间检查任务和租约期限。
	var databaseTime time.Time
	queryError := store.database.WithContext(renewContext).Raw("SELECT UTC_TIMESTAMP(6)").Row().Scan(&databaseTime)
	if err := renewContext.Err(); err != nil {
		return nil, err
	}
	if queryError != nil {
		return nil, fmt.Errorf("读取上传续租数据库时间失败: %w", queryError)
	}
	databaseTime = databaseTime.UTC()
	nextDeadline := databaseTime.Add(time.Minute)
	if databaseTime.Year() < 1000 || nextDeadline.Year() > 9999 || databaseTime.Nanosecond()%1000 != 0 {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if !databaseTime.Before(task.ExpiresAt) {
		return nil, domain.ErrUploadTaskNotReady
	}
	if !databaseTime.Before(*task.LeaseUntil) {
		return nil, domain.ErrUploadTaskBusy
	}
	if !nextDeadline.After(*task.LeaseUntil) {
		return task, nil
	}

	// 4. 按当前租约条件延长期限，复核更新结果。
	result := store.database.WithContext(renewContext).Exec(
		"UPDATE upload_tasks SET lease_until = DATE_ADD(UTC_TIMESTAMP(6), INTERVAL 60 SECOND), updated_at = UTC_TIMESTAMP(6) "+
			"WHERE user_id = ? AND id = ? AND staged_object_id = ? AND lease_version = ? AND lease_owner = ? "+
			"AND status = 'UPLOADING' AND upload_mode = 'SINGLE' AND reserved_bytes = file_size "+
			"AND expires_at > UTC_TIMESTAMP(6) AND lease_until > UTC_TIMESTAMP(6) "+
			"AND lease_until < DATE_ADD(UTC_TIMESTAMP(6), INTERVAL 60 SECOND)",
		task.UserID, taskID, objectID, leaseVersion, leaseOwner)
	if err := renewContext.Err(); err != nil {
		return nil, err
	}
	if result.Error != nil {
		return nil, fmt.Errorf("续租上传接收失败: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return nil, domain.ErrUploadTaskBusy
	}
	if result.RowsAffected != 1 {
		return nil, domain.ErrInvalidUploadTaskData
	}
	renewed, err := store.LockUserUploadTask(renewContext, taskID)
	if errors.Is(err, domain.ErrUploadTaskNotFound) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if err != nil {
		return nil, err
	}
	if renewed.StagedObjectID != objectID || renewed.Status != task.Status || renewed.UploadMode != task.UploadMode ||
		renewed.LeaseVersion != leaseVersion || renewed.LeaseOwner == nil || *renewed.LeaseOwner != leaseOwner ||
		renewed.LeaseUntil == nil || !renewed.LeaseUntil.After(*task.LeaseUntil) ||
		!renewed.LeaseUntil.Equal(renewed.UpdatedAt.Add(time.Minute)) || renewed.UpdatedAt.Before(databaseTime) ||
		renewed.UpdatedAt.Before(task.UpdatedAt) || !renewed.UpdatedAt.Before(*task.LeaseUntil) ||
		!renewed.ExpiresAt.Equal(task.ExpiresAt) || !renewed.UpdatedAt.Before(renewed.ExpiresAt) ||
		renewed.ReservedBytes != task.ReservedBytes || renewed.FileSize != task.FileSize {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if err := renewContext.Err(); err != nil {
		return nil, err
	}
	return renewed, nil
}
