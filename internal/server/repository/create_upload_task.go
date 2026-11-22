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
	return store.createUserUploadTask(ctx, stagedObjectID, input, domain.UploadPlan{Mode: "SINGLE"}, expiresAt, nil)
}

// CreateUserMultipartUploadTask 在 WithUserTransaction 回调中登记当前设备的 CREATING 任务。
// 调用方须使用已通过限制校验的计划，在同一事务中编排祖先可见性、配额预占及幂等记录。
func (store *Store) CreateUserMultipartUploadTask(ctx context.Context, stagedObjectID uint64, input domain.CreateUploadInput, plan domain.UploadPlan, expiresAt time.Time) (*domain.UploadTask, error) {
	if plan.Mode != "MULTIPART" {
		return nil, domain.ErrInvalidUploadTaskData
	}
	return store.createUserUploadTask(ctx, stagedObjectID, input, plan, expiresAt, nil)
}

// CreateUserUploadReplacementTask 在已认证账号事务内写入带绑定的初始任务。
// 调用方提供同一事务准备的父目录、名称和固定计划，并在同一事务编排配额预占及回执。
func (store *Store) CreateUserUploadReplacementTask(ctx context.Context, stagedObjectID uint64,
	input domain.CreateUploadInput, plan domain.UploadPlan, entryID, expectedRevision uint64, expiresAt time.Time,
) (*domain.UploadTask, error) {
	binding := uploadReplacementBinding{entryID: entryID, expectedRevision: expectedRevision}
	return store.createUserUploadTask(ctx, stagedObjectID, input, plan, expiresAt, &binding)
}

// uploadReplacementBinding 保存普通任务与替换任务之间唯一额外的创建绑定。
type uploadReplacementBinding struct {
	entryID          uint64
	expectedRevision uint64
}

func (store *Store) createUserUploadTask(ctx context.Context, stagedObjectID uint64, input domain.CreateUploadInput,
	plan domain.UploadPlan, expiresAt time.Time, replacement *uploadReplacementBinding,
) (*domain.UploadTask, error) {
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

	// 2. 校验声明、对象 ID 及到期时间，按模式准备初始状态、分片参数及替换绑定。
	expiry := expiresAt.UTC().Truncate(time.Microsecond)
	if stagedObjectID == 0 || expiry.Year() < 1000 || expiry.Year() > 9999 || !expiry.After(time.Now()) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	record := models.NewUploadTask{
		UserID:         actor.UserID,
		DeviceID:       actor.DeviceID,
		StagedObjectID: stagedObjectID,
		TargetParentID: input.ParentID,
		RequestedName:  input.Name,
		UploadMode:     plan.Mode,
		FileSize:       input.SizeBytes,
		Status:         string(domain.UploadStatusUploading),
		ReservedBytes:  input.SizeBytes,
		ExpiresAt:      expiry,
	}
	if plan.Mode == "MULTIPART" {
		if !input.IsValidMultipart(plan) {
			return nil, domain.ErrInvalidUploadTaskData
		}
		record.Status = string(domain.UploadStatusCreating)
		record.PartSize, record.PartCount = &plan.PartSizeBytes, &plan.PartCount
	} else if plan != (domain.UploadPlan{Mode: "SINGLE"}) || !input.IsValid() {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if input.ExpectedSHA256 != nil {
		record.ExpectedSHA256 = bytes.Clone(input.ExpectedSHA256[:])
	}
	if replacement != nil && (replacement.entryID == 0 || replacement.expectedRevision == 0 || input.ExpectedSHA256 == nil) {
		return nil, domain.ErrInvalidUploadTaskData
	}

	// 3. 复核替换文件及其快照，按用户归属核对目录和初始暂存对象。
	if replacement != nil {
		target, err := store.LockUploadReplacementTarget(writeContext, replacement.entryID, replacement.expectedRevision)
		if err != nil {
			return nil, err
		}
		if target.ParentID == nil || *target.ParentID != input.ParentID || target.Name != input.Name {
			return nil, domain.ErrInvalidUploadTaskData
		}
	}
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

	// 4. 按模式插入任务，按需写入分片列和替换绑定。
	columns := []string{"user_id", "device_id", "staged_object_id", "target_parent_id", "requested_name", "upload_mode",
		"expected_sha256", "file_size", "status", "reserved_bytes", "expires_at"}
	if record.PartSize != nil {
		columns = append(columns, "part_size", "part_count")
	}
	insert := store.database.WithContext(writeContext)
	var result *gorm.DB
	if replacement == nil {
		result = insert.Select(columns).Create(&record)
	} else {
		replacementRecord := models.NewUploadReplacementTask{
			NewUploadTask:               record,
			ReplacementEntryID:          replacement.entryID,
			ReplacementExpectedRevision: replacement.expectedRevision,
		}
		result = insert.Select(append(columns, "replacement_entry_id", "replacement_expected_revision")).Create(&replacementRecord)
	}
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

	// 6. 复用任务读取，核对完整初始事实及替换绑定。
	created, err := store.FindUserUploadTask(writeContext, actor.UserID, identity.ID)
	if errors.Is(err, domain.ErrUploadTaskNotFound) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if err != nil {
		return nil, fmt.Errorf("读取新建上传任务失败: %w", err)
	}
	if created.DeviceID != actor.DeviceID || created.StagedObjectID != stagedObjectID || created.TargetParentID != input.ParentID ||
		created.RequestedName != input.Name || created.FileSize != input.SizeBytes || created.UploadMode != plan.Mode ||
		string(created.Status) != record.Status || created.ReservedBytes != input.SizeBytes || !created.ExpiresAt.Equal(expiry) ||
		(created.ExpectedSHA256 == nil) != (record.ExpectedSHA256 == nil) || created.PublishedObjectID != nil || created.ResolvedName != nil ||
		created.ResultEntryID != nil || created.StorageUploadID != nil ||
		created.LeaseOwner != nil || created.LeaseUntil != nil || created.LeaseVersion != 0 || created.NextAttemptAt != nil ||
		created.AttemptCount != 0 || created.LastErrorCode != nil || created.LastErrorMessage != nil || created.CompletedAt != nil {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if record.PartSize != nil {
		if created.PartSize == nil || *created.PartSize != *record.PartSize ||
			created.PartCount == nil || *created.PartCount != *record.PartCount {
			return nil, domain.ErrInvalidUploadTaskData
		}
	} else if created.PartSize != nil || created.PartCount != nil {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if created.ExpectedSHA256 != nil && !bytes.Equal(created.ExpectedSHA256[:], record.ExpectedSHA256) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if replacement == nil {
		if created.ReplacementEntryID != nil {
			return nil, domain.ErrInvalidUploadTaskData
		}
	} else if created.ReplacementEntryID == nil || *created.ReplacementEntryID != replacement.entryID ||
		created.ReplacementExpectedRevision == nil || *created.ReplacementExpectedRevision != replacement.expectedRevision {
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
