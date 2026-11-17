package repository

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
	"gorm.io/gorm"
)

// CreateUserUploadTask 在 WithUserTransaction 回调中创建当前设备的 SINGLE 任务。
// 声明名称须已通过统一校验，祖先可见性、配额预占及幂等结果由上层在同一事务编排。
// 回调须向外返回本方法错误，结果在外层事务提交成功后确认生效。
func (store *Store) CreateUserUploadTask(ctx context.Context, stagedObjectID uint64, input domain.CreateUploadInput, expiresAt time.Time) (*domain.UploadTask, error) {
	// 1. 检查已认证用户事务和上下文。
	if ctx == nil || store.authenticatedActor == nil || store.loginUserID == 0 {
		return nil, fmt.Errorf("创建上传任务需要上下文和已认证用户事务")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return nil, fmt.Errorf("创建上传任务必须使用已认证事务 Store")
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

	// 2. 校验声明、对象 ID 及到期时间，复制摘要。
	expiry := expiresAt.UTC().Truncate(time.Microsecond)
	if stagedObjectID == 0 || !input.IsValid() || expiry.Year() < 1000 || expiry.Year() > 9999 || !expiry.After(time.Now()) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	record := models.NewUploadTask{
		UserID:         actor.UserID,
		DeviceID:       actor.DeviceID,
		StagedObjectID: stagedObjectID,
		TargetParentID: input.ParentID,
		RequestedName:  input.Name,
		UploadMode:     "SINGLE",
		FileSize:       input.SizeBytes,
		Status:         string(domain.UploadStatusUploading),
		ReservedBytes:  input.SizeBytes,
		ExpiresAt:      expiry,
	}
	if input.ExpectedSHA256 != nil {
		record.ExpectedSHA256 = bytes.Clone(input.ExpectedSHA256[:])
	}

	// 3. 按用户归属核对目标目录和初始暂存对象。
	parent, err := store.FindUserFileEntry(writeContext, actor.UserID, input.ParentID)
	if err != nil {
		return nil, err
	}
	if parent.Status != "ACTIVE" {
		return nil, domain.ErrFileEntryNotFound
	}
	if parent.EntryType != "DIR" {
		return nil, domain.ErrFileEntryNotDirectory
	}
	if parent.FileSize != 0 {
		return nil, domain.ErrInvalidFileEntryData
	}
	staged, err := store.FindUserStoredObject(writeContext, actor.UserID, stagedObjectID)
	if err != nil {
		return nil, err
	}
	if staged.Status != domain.StoredObjectStatusStaging || staged.FileSize != input.SizeBytes ||
		staged.StorageVersionID != nil || staged.VerifiedSHA256 != nil || staged.DedupHash != nil || staged.StorageETag != nil ||
		staged.VerifiedAt != nil || staged.UnreferencedAt != nil || staged.GCAfter != nil || staged.LastErrorCode != nil {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if !expiry.After(time.Now()) {
		return nil, domain.ErrInvalidUploadTaskData
	}

	// 4. 插入 SINGLE 上传任务，处理取消、重复引用和故障。
	result := store.database.WithContext(writeContext).
		Select("user_id", "device_id", "staged_object_id", "target_parent_id", "requested_name", "upload_mode",
			"expected_sha256", "file_size", "status", "reserved_bytes", "expires_at").Create(&record)
	if err := writeContext.Err(); err != nil {
		return nil, err
	}
	if result.Error != nil {
		var databaseError *mysql.MySQLError
		if errors.As(result.Error, &databaseError) && databaseError.Number == 1062 {
			return nil, domain.ErrUploadTaskStagedObjectConflict
		}
		return nil, fmt.Errorf("创建上传任务失败: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return nil, domain.ErrInvalidUploadTaskData
	}

	// 5. 按用户与暂存对象唯一键取得任务标识。
	var identity models.UploadTask
	queryError := store.database.WithContext(writeContext).
		Select("id", "user_id").Where("user_id = ? AND staged_object_id = ?", actor.UserID, stagedObjectID).Take(&identity).Error
	if err := writeContext.Err(); err != nil {
		return nil, err
	}
	if errors.Is(queryError, gorm.ErrRecordNotFound) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if queryError != nil {
		return nil, fmt.Errorf("定位新建上传任务失败: %w", queryError)
	}
	if identity.ID == 0 || identity.UserID != actor.UserID {
		return nil, domain.ErrInvalidUploadTaskData
	}

	// 6. 复用任务读取，核对完整初始事实。
	created, err := store.FindUserUploadTask(writeContext, actor.UserID, identity.ID)
	if errors.Is(err, domain.ErrUploadTaskNotFound) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if err != nil {
		return nil, fmt.Errorf("读取新建上传任务失败: %w", err)
	}
	if created.DeviceID != actor.DeviceID || created.StagedObjectID != stagedObjectID || created.TargetParentID != input.ParentID ||
		created.RequestedName != input.Name || created.FileSize != input.SizeBytes || created.UploadMode != "SINGLE" ||
		created.Status != domain.UploadStatusUploading || created.ReservedBytes != input.SizeBytes || !created.ExpiresAt.Equal(expiry) ||
		(created.ExpectedSHA256 == nil) != (record.ExpectedSHA256 == nil) || created.PublishedObjectID != nil || created.ResolvedName != nil ||
		created.ResultEntryID != nil || created.StorageUploadID != nil || created.PartSize != nil || created.PartCount != nil ||
		created.LeaseOwner != nil || created.LeaseUntil != nil || created.LeaseVersion != 0 || created.NextAttemptAt != nil ||
		created.AttemptCount != 0 || created.LastErrorCode != nil || created.LastErrorMessage != nil || created.CompletedAt != nil {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if created.ExpectedSHA256 != nil && !bytes.Equal(created.ExpectedSHA256[:], record.ExpectedSHA256) {
		return nil, domain.ErrInvalidUploadTaskData
	}

	// 7. 复查上下文和到期时间，交由外层事务确认。
	if err := writeContext.Err(); err != nil {
		return nil, err
	}
	if !expiry.After(time.Now()) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	return created, nil
}
