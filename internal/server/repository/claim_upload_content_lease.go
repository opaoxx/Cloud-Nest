package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
)

// ClaimUserUploadContentLease 在已认证用户事务中条件领取六十秒内容接收租约。
// 预期对象和版本来自当前任务，持有者由服务端生成，每次接收使用独立标识。
// 上层在同一事务编排重试对象替换，回调须向外返回错误，结果由外层提交确认。
func (store *Store) ClaimUserUploadContentLease(ctx context.Context, taskID, stagedObjectID, expectedVersion uint64, leaseOwner string) (*domain.UploadTask, error) {
	// 1. 检查已认证事务、任务范围和新租约持有者。
	if ctx == nil || taskID == 0 || stagedObjectID == 0 || store.authenticatedActor == nil || store.loginUserID == 0 {
		return nil, fmt.Errorf("领取上传接收租约需要上下文、非零任务及对象 ID 和已认证用户事务")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return nil, fmt.Errorf("领取上传接收租约必须使用已认证事务 Store")
	}
	actor := *store.authenticatedActor
	if actor.UserID != store.loginUserID || actor.DeviceID == 0 || actor.SessionID == 0 {
		return nil, domain.ErrInvalidSessionData
	}
	writeContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := writeContext.Err(); err != nil {
		return nil, err
	}
	if len(leaseOwner) == 0 || len(leaseOwner) > 64 {
		return nil, domain.ErrInvalidUploadTaskData
	}
	for _, character := range leaseOwner {
		if character > 127 {
			return nil, domain.ErrInvalidUploadTaskData
		}
	}

	// 2. 加锁读取任务，复核接收阶段和预期对象、版本。
	task, err := store.LockUserUploadTask(writeContext, taskID)
	if err != nil {
		return nil, err
	}
	if task.UploadMode != "SINGLE" || task.Status != domain.UploadStatusUploading {
		return nil, domain.ErrUploadTaskNotReady
	}
	if task.FileSize > domain.MaxSingleUploadSizeBytes {
		return nil, domain.ErrUploadFileTooLarge
	}
	if task.ReservedBytes != task.FileSize || task.PublishedObjectID != nil || task.ResolvedName != nil || task.ResultEntryID != nil || task.CompletedAt != nil ||
		(task.LeaseOwner != nil && (len(*task.LeaseOwner) == 0 || task.LeaseVersion == 0)) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if task.StagedObjectID != stagedObjectID || task.LeaseVersion != expectedVersion {
		return nil, domain.ErrUploadTaskBusy
	}

	// 3. 读取数据库时间，检查任务期限、占用和版本上限。
	var databaseTime time.Time
	queryError := store.database.WithContext(writeContext).Raw("SELECT UTC_TIMESTAMP(6)").Row().Scan(&databaseTime)
	if err := writeContext.Err(); err != nil {
		return nil, err
	}
	if queryError != nil {
		return nil, fmt.Errorf("读取上传租约数据库时间失败: %w", queryError)
	}
	databaseTime = databaseTime.UTC()
	if databaseTime.Year() < 1000 || databaseTime.Add(time.Minute).Year() > 9999 || databaseTime.Nanosecond()%1000 != 0 {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if !databaseTime.Before(task.ExpiresAt) {
		return nil, domain.ErrUploadTaskNotReady
	}
	if task.LeaseUntil != nil && databaseTime.Before(*task.LeaseUntil) {
		return nil, domain.ErrUploadTaskBusy
	}
	if expectedVersion == ^uint64(0) {
		return nil, domain.ErrUploadLeaseVersionExhausted
	}

	// 4. 按当前对象、版本及有效期限条件领取六十秒租约。
	result := store.database.WithContext(writeContext).Exec(
		"UPDATE upload_tasks SET lease_owner = ?, lease_until = DATE_ADD(UTC_TIMESTAMP(6), INTERVAL 60 SECOND), "+
			"lease_version = lease_version + 1, updated_at = UTC_TIMESTAMP(6) "+
			"WHERE user_id = ? AND id = ? AND staged_object_id = ? AND lease_version = ? AND lease_version < ? "+
			"AND status = 'UPLOADING' AND upload_mode = 'SINGLE' AND expires_at > UTC_TIMESTAMP(6) "+
			"AND (lease_until IS NULL OR lease_until <= UTC_TIMESTAMP(6))",
		leaseOwner, actor.UserID, taskID, stagedObjectID, expectedVersion, ^uint64(0))

	// 5. 检查更新结果，再以当前读核对新租约。
	if err := writeContext.Err(); err != nil {
		return nil, err
	}
	if result.Error != nil {
		return nil, fmt.Errorf("领取上传接收租约失败: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return nil, domain.ErrUploadTaskNotReady
	}
	if result.RowsAffected != 1 {
		return nil, domain.ErrInvalidUploadTaskData
	}
	claimed, err := store.LockUserUploadTask(writeContext, taskID)
	if errors.Is(err, domain.ErrUploadTaskNotFound) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if err != nil {
		return nil, err
	}
	if claimed.StagedObjectID != stagedObjectID || claimed.UploadMode != "SINGLE" || claimed.Status != domain.UploadStatusUploading ||
		claimed.LeaseOwner == nil || *claimed.LeaseOwner != leaseOwner || claimed.LeaseUntil == nil || claimed.LeaseVersion != expectedVersion+1 ||
		claimed.UpdatedAt.Before(databaseTime) || !claimed.UpdatedAt.Before(claimed.ExpiresAt) || !claimed.LeaseUntil.Equal(claimed.UpdatedAt.Add(time.Minute)) {
		return nil, domain.ErrInvalidUploadTaskData
	}

	// 6. 返回事务内结果，由外层提交确认。
	if err := writeContext.Err(); err != nil {
		return nil, err
	}
	return claimed, nil
}
