package repository

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// SettlePublishedUploadQuota 将本次发布的任务预占等额转为用户已用字节。
// 在写回复核事务中创建文件后调用，文件写入、配额结算和任务完成由外层一起提交。
func (store *Store) SettlePublishedUploadQuota(ctx context.Context, task *domain.UploadTask) error {
	// 1. 核对发布事务和当前任务的校验租约。
	if store == nil || store.database == nil || store.loginUserID == 0 || ctx == nil {
		return fmt.Errorf("结算上传配额需要上下文和已锁定用户的事务")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return fmt.Errorf("结算上传配额必须使用事务 Store")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	userID := store.loginUserID
	lockedStore := *store
	lockedStore.database = store.database.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"})
	current, err := lockedStore.findVerifyingUploadWithFullReservation(ctx, task, "SINGLE")
	if err != nil {
		return err
	}

	// 2. 读取用户配额，检查任务预占和可结算金额。
	account, err := store.lockUserUploadQuota(ctx, userID)
	if err != nil {
		return err
	}
	uploadBytes := current.ReservedBytes
	if uploadBytes > account.ReservedBytes {
		return domain.ErrInvalidAccountData
	}
	// 已用加预占在配额内，且本次结算额不超过预占，以下两项运算都在 uint64 范围内。
	usedBytes := account.UsedBytes + uploadBytes
	reservedBytes := account.ReservedBytes - uploadBytes

	// 3. 用数据库时间校验期限，零字节直接通过。
	var now time.Time
	clockError := store.database.WithContext(ctx).Raw("SELECT UTC_TIMESTAMP(6)").Row().Scan(&now)
	if err := ctx.Err(); err != nil {
		return err
	}
	if clockError != nil {
		return fmt.Errorf("读取上传配额结算时间失败: %w", clockError)
	}
	now = now.UTC()
	if now.Year() < 1000 || now.Year() > 9999 || now.Nanosecond()%1000 != 0 {
		return domain.ErrInvalidUploadTaskData
	}
	if !now.Before(current.ExpiresAt) {
		return domain.ErrUploadTaskNotReady
	}
	if !now.Before(*current.LeaseUntil) {
		return domain.ErrUploadTaskBusy
	}
	if uploadBytes == 0 {
		return nil
	}

	// 4. 条件更新两张表，交由外层事务提交或回滚。
	result := store.database.WithContext(ctx).Exec(
		"UPDATE users AS quota_user JOIN upload_tasks AS upload_task ON upload_task.user_id = quota_user.id "+
			"SET quota_user.used_storage = ?, quota_user.reserved_storage = ?, quota_user.updated_at = UTC_TIMESTAMP(6), "+
			"upload_task.reserved_bytes = 0, upload_task.updated_at = UTC_TIMESTAMP(6) "+
			"WHERE quota_user.id = ? AND quota_user.status = 'ACTIVE' "+
			"AND quota_user.storage_quota = ? AND quota_user.used_storage = ? AND quota_user.reserved_storage = ? "+
			"AND upload_task.id = ? AND upload_task.staged_object_id = ? "+
			"AND CAST(upload_task.lease_owner AS BINARY) = ? AND upload_task.lease_version = ? "+
			"AND upload_task.lease_until > UTC_TIMESTAMP(6) AND upload_task.expires_at > UTC_TIMESTAMP(6) "+
			"AND upload_task.status = 'VERIFYING' AND upload_task.upload_mode = 'SINGLE' "+
			"AND upload_task.file_size = ? AND upload_task.reserved_bytes = upload_task.file_size "+
			"AND upload_task.published_object_id IS NULL AND upload_task.result_entry_id IS NULL "+
			"AND upload_task.resolved_name IS NULL AND upload_task.completed_at IS NULL",
		usedBytes, reservedBytes, userID, account.QuotaBytes, account.UsedBytes, account.ReservedBytes,
		current.ID, current.StagedObjectID, *current.LeaseOwner, current.LeaseVersion, uploadBytes)
	if err := ctx.Err(); err != nil {
		return err
	}
	if result.Error != nil {
		return fmt.Errorf("结算上传配额失败: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return domain.ErrUploadTaskBusy
	}
	if result.RowsAffected != 2 {
		return domain.ErrInvalidUploadTaskData
	}
	return nil
}

// findVerifyingUploadWithFullReservation 复核待发布任务及其完整预占，调用方提供加锁读取的 Store。
func (lockedStore *Store) findVerifyingUploadWithFullReservation(ctx context.Context, task *domain.UploadTask, uploadMode string) (*domain.UploadTask, error) {
	// 1. 校验事务账号和传入的租约标识。
	userID := lockedStore.loginUserID
	if task == nil || task.ID == 0 || task.UserID != userID || task.StagedObjectID == 0 ||
		task.LeaseVersion == 0 || task.LeaseOwner == nil || *task.LeaseOwner == "" || task.LeaseUntil == nil {
		return nil, domain.ErrInvalidUploadTaskData
	}

	// 2. 加锁读回当前任务，核对阶段、租约和预占。
	current, err := lockedStore.FindUserUploadTask(ctx, userID, task.ID)
	if err != nil {
		return nil, err
	}
	if current.Status != domain.UploadStatusVerifying || current.UploadMode != uploadMode || task.UploadMode != uploadMode {
		return nil, domain.ErrUploadTaskNotReady
	}
	if current.StagedObjectID != task.StagedObjectID || current.LeaseVersion != task.LeaseVersion ||
		current.LeaseOwner == nil || *current.LeaseOwner != *task.LeaseOwner || current.LeaseUntil == nil ||
		(uploadMode == "MULTIPART" && !sameUploadStorageText(current.StorageUploadID, task.StorageUploadID)) {
		return nil, domain.ErrUploadTaskBusy
	}
	if current.FileSize != task.FileSize || current.ReservedBytes != current.FileSize ||
		current.PublishedObjectID != nil || current.ResultEntryID != nil || current.ResolvedName != nil || current.CompletedAt != nil {
		return nil, domain.ErrInvalidUploadTaskData
	}
	maxSize := domain.MaxSingleUploadSizeBytes
	if uploadMode == "MULTIPART" {
		maxSize = math.MaxInt64
		if task.StorageUploadID == nil || !domain.ValidMultipartUploadID(*task.StorageUploadID) ||
			task.ExpectedSHA256 == nil || task.PartSize == nil || task.PartCount == nil ||
			!domain.ValidMultipartUploadLayout(task.FileSize, *task.PartSize, *task.PartCount) ||
			!sameMultipartUploadPlan(task, current) {
			return nil, domain.ErrInvalidUploadTaskData
		}
	}
	if current.FileSize > maxSize {
		return nil, domain.ErrUploadFileTooLarge
	}
	return current, nil
}
