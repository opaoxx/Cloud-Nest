package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ReplaceUserUploadStagedObject 在已认证事务中用当前接收租约替换任务的暂存对象。
// 新对象由上层在同一事务创建，旧对象孤立标记继续在同一事务中编排。
// 回调须向外返回错误，返回任务由外层提交确认。
func (store *Store) ReplaceUserUploadStagedObject(ctx context.Context, taskID, oldObjectID, newObjectID, leaseVersion uint64, leaseOwner string) (*domain.UploadTask, error) {
	// 1. 检查已认证事务、任务及对象标识和租约参数。
	if ctx == nil || taskID == 0 || oldObjectID == 0 || newObjectID == 0 || store.authenticatedActor == nil || store.loginUserID == 0 {
		return nil, fmt.Errorf("替换上传暂存对象需要上下文、非零任务及对象 ID 和已认证用户事务")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return nil, fmt.Errorf("替换上传暂存对象必须使用已认证事务 Store")
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
	if oldObjectID == newObjectID || leaseVersion == 0 || len(leaseOwner) == 0 || len(leaseOwner) > 64 {
		return nil, domain.ErrInvalidUploadTaskData
	}
	for _, character := range leaseOwner {
		if character > 127 {
			return nil, domain.ErrInvalidUploadTaskData
		}
	}

	// 2. 加锁读取任务，复核接收阶段、旧对象和当前租约。
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
	if task.ReservedBytes != task.FileSize || task.PublishedObjectID != nil || task.ResolvedName != nil || task.ResultEntryID != nil || task.CompletedAt != nil {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if task.StagedObjectID != oldObjectID || task.LeaseVersion != leaseVersion || task.LeaseOwner == nil || task.LeaseUntil == nil || *task.LeaseOwner != leaseOwner {
		return nil, domain.ErrUploadTaskBusy
	}

	// 3. 当前读核对新旧暂存对象及新对象的初始事实。
	lockedStore := *store
	lockedStore.database = store.database.WithContext(writeContext).Clauses(clause.Locking{Strength: "UPDATE"})
	oldObject, err := lockedStore.FindUserStoredObject(writeContext, actor.UserID, oldObjectID)
	if err != nil {
		return nil, err
	}
	newObject, err := lockedStore.FindUserStoredObject(writeContext, actor.UserID, newObjectID)
	if err != nil {
		return nil, err
	}
	if oldObject.Status != domain.StoredObjectStatusStaging || oldObject.FileSize != task.FileSize || oldObject.UnreferencedAt != nil ||
		newObject.Status != domain.StoredObjectStatusStaging || newObject.FileSize != task.FileSize ||
		newObject.StorageVersionID != nil || newObject.VerifiedSHA256 != nil || newObject.DedupHash != nil || newObject.StorageETag != nil ||
		newObject.VerifiedAt != nil || newObject.UnreferencedAt != nil || newObject.GCAfter != nil || newObject.LastErrorCode != nil ||
		(oldObject.Provider == newObject.Provider && oldObject.Bucket == newObject.Bucket && oldObject.ObjectKey == newObject.ObjectKey) {
		return nil, domain.ErrInvalidStoredObjectData
	}

	// 4. 用数据库时间检查任务期限和租约有效性。
	var databaseTime time.Time
	queryError := store.database.WithContext(writeContext).Raw("SELECT UTC_TIMESTAMP(6)").Row().Scan(&databaseTime)
	if err := writeContext.Err(); err != nil {
		return nil, err
	}
	if queryError != nil {
		return nil, fmt.Errorf("读取暂存对象替换数据库时间失败: %w", queryError)
	}
	databaseTime = databaseTime.UTC()
	if databaseTime.Year() < 1000 || databaseTime.Year() > 9999 || databaseTime.Nanosecond()%1000 != 0 {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if !databaseTime.Before(task.ExpiresAt) {
		return nil, domain.ErrUploadTaskNotReady
	}
	if !databaseTime.Before(*task.LeaseUntil) {
		return nil, domain.ErrUploadTaskBusy
	}

	// 5. 按旧对象、持有者、版本及期限条件替换引用。
	result := store.database.WithContext(writeContext).Exec(
		"UPDATE upload_tasks SET staged_object_id = ?, updated_at = UTC_TIMESTAMP(6) "+
			"WHERE user_id = ? AND id = ? AND staged_object_id = ? AND lease_owner = ? AND lease_version = ? "+
			"AND lease_until > UTC_TIMESTAMP(6) AND expires_at > UTC_TIMESTAMP(6) AND status = 'UPLOADING' AND upload_mode = 'SINGLE'",
		newObjectID, actor.UserID, taskID, oldObjectID, leaseOwner, leaseVersion)
	if err := writeContext.Err(); err != nil {
		return nil, err
	}
	if result.Error != nil {
		var databaseError *mysql.MySQLError
		if errors.As(result.Error, &databaseError) && databaseError.Number == 1062 {
			return nil, domain.ErrUploadTaskStagedObjectConflict
		}
		return nil, fmt.Errorf("替换上传暂存对象失败: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return nil, domain.ErrUploadTaskBusy
	}
	if result.RowsAffected != 1 {
		return nil, domain.ErrInvalidUploadTaskData
	}

	// 6. 当前读复核新引用和租约，交由外层事务确认。
	replaced, err := store.LockUserUploadTask(writeContext, taskID)
	if errors.Is(err, domain.ErrUploadTaskNotFound) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if err != nil {
		return nil, err
	}
	if replaced.StagedObjectID != newObjectID || replaced.UploadMode != "SINGLE" || replaced.Status != domain.UploadStatusUploading ||
		replaced.FileSize != task.FileSize || replaced.ReservedBytes != task.ReservedBytes || !replaced.ExpiresAt.Equal(task.ExpiresAt) ||
		replaced.LeaseOwner == nil || *replaced.LeaseOwner != leaseOwner || replaced.LeaseVersion != leaseVersion || replaced.LeaseUntil == nil ||
		!replaced.LeaseUntil.Equal(*task.LeaseUntil) || replaced.UpdatedAt.Before(databaseTime) ||
		!replaced.UpdatedAt.Before(replaced.ExpiresAt) || !replaced.UpdatedAt.Before(*replaced.LeaseUntil) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if err := writeContext.Err(); err != nil {
		return nil, err
	}
	return replaced, nil
}
