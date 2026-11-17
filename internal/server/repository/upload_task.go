package repository

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
	"gorm.io/gorm"
)

// FindUserUploadTask 按可信用户归属读取上传任务，保留到期及终态记录。
// 缺失返回 ErrUploadTaskNotFound，异常记录返回 ErrInvalidUploadTaskData。
// 返回值独立持有可空字段，后续业务写入复用已认证用户事务中的 Store。
func (store *Store) FindUserUploadTask(ctx context.Context, userID, taskID uint64) (*domain.UploadTask, error) {
	// 1. 检查查询范围和上下文，设置最多五秒期限。
	if ctx == nil || userID == 0 || taskID == 0 {
		return nil, fmt.Errorf("查询上传任务需要上下文、非零用户 ID 和任务 ID")
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("查询上传任务失败: %w", err)
	}

	// 2. 绑定用户与任务 ID，读取明确的二十八列。
	var record models.UploadTask
	queryError := store.database.WithContext(queryContext).
		Select("id", "user_id", "device_id", "staged_object_id", "published_object_id", "target_parent_id",
			"requested_name", "resolved_name", "upload_mode", "storage_upload_id", "expected_sha256",
			"file_size", "part_size", "part_count", "status", "result_entry_id", "reserved_bytes",
			"lease_owner", "lease_until", "lease_version", "next_attempt_at", "attempt_count", "expires_at",
			"last_error_code", "last_error_message", "created_at", "updated_at", "completed_at").
		Where("user_id = ? AND id = ?", userID, taskID).Take(&record).Error

	// 3. 处理取消、未找到及数据库故障。
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("查询上传任务已取消: %w", err)
	}
	if errors.Is(queryError, gorm.ErrRecordNotFound) {
		return nil, domain.ErrUploadTaskNotFound
	}
	if queryError != nil {
		return nil, fmt.Errorf("查询上传任务失败: %w", queryError)
	}

	// 4. 核对任务归属、声明和状态约束。
	status := domain.UploadStatus(record.Status)
	if record.ID != taskID || record.UserID != userID || record.DeviceID == 0 || record.StagedObjectID == 0 ||
		record.TargetParentID == 0 || !status.IsValid() || record.ReservedBytes > record.FileSize ||
		(record.ExpectedSHA256 != nil && len(record.ExpectedSHA256) != sha256.Size) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	for _, identifier := range []*uint64{record.PublishedObjectID, record.ResultEntryID} {
		if identifier != nil && *identifier == 0 {
			return nil, domain.ErrInvalidUploadTaskData
		}
	}
	if record.RequestedName == "" || (record.ResolvedName != nil && *record.ResolvedName == "") {
		return nil, domain.ErrInvalidUploadTaskData
	}
	for _, text := range []*string{&record.RequestedName, record.ResolvedName, record.StorageUploadID, record.LeaseOwner, record.LastErrorCode, record.LastErrorMessage} {
		if text != nil && !utf8.ValidString(*text) {
			return nil, domain.ErrInvalidUploadTaskData
		}
	}
	switch record.UploadMode {
	case "SINGLE":
		if record.PartSize != nil || record.PartCount != nil || record.StorageUploadID != nil {
			return nil, domain.ErrInvalidUploadTaskData
		}
	case "MULTIPART":
		if record.PartSize == nil || *record.PartSize < 5242880 || record.PartCount == nil || *record.PartCount < 1 || *record.PartCount > 10000 {
			return nil, domain.ErrInvalidUploadTaskData
		}
	default:
		return nil, domain.ErrInvalidUploadTaskData
	}
	if status.IsTerminal() && record.ReservedBytes != 0 {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if status == domain.UploadStatusCompleted && (record.PublishedObjectID == nil || record.ResultEntryID == nil || record.CompletedAt == nil) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if (record.LeaseOwner == nil) != (record.LeaseUntil == nil) {
		return nil, domain.ErrInvalidUploadTaskData
	}

	// 5. 核对生命周期时间，复制摘要及可空字段。
	for _, stamp := range []*time.Time{&record.ExpiresAt, &record.CreatedAt, &record.UpdatedAt, record.LeaseUntil, record.NextAttemptAt, record.CompletedAt} {
		if stamp != nil && (stamp.UTC().Year() < 1000 || stamp.UTC().Year() > 9999 || stamp.Nanosecond()%1000 != 0) {
			return nil, domain.ErrInvalidUploadTaskData
		}
	}
	var expectedSHA256 *[sha256.Size]byte
	if record.ExpectedSHA256 != nil {
		expectedSHA256 = new([sha256.Size]byte)
		copy(expectedSHA256[:], record.ExpectedSHA256)
	}
	result := &domain.UploadTask{
		ID:                record.ID,
		UserID:            record.UserID,
		DeviceID:          record.DeviceID,
		StagedObjectID:    record.StagedObjectID,
		TargetParentID:    record.TargetParentID,
		RequestedName:     record.RequestedName,
		UploadMode:        record.UploadMode,
		StorageUploadID:   copyUploadTaskValue(record.StorageUploadID),
		ExpectedSHA256:    expectedSHA256,
		FileSize:          record.FileSize,
		PartSize:          copyUploadTaskValue(record.PartSize),
		PartCount:         copyUploadTaskValue(record.PartCount),
		Status:            status,
		PublishedObjectID: copyUploadTaskValue(record.PublishedObjectID),
		ResolvedName:      copyUploadTaskValue(record.ResolvedName),
		ResultEntryID:     copyUploadTaskValue(record.ResultEntryID),
		ReservedBytes:     record.ReservedBytes,
		LeaseOwner:        copyUploadTaskValue(record.LeaseOwner),
		LeaseUntil:        copyUploadTaskValue(record.LeaseUntil),
		LeaseVersion:      record.LeaseVersion,
		NextAttemptAt:     copyUploadTaskValue(record.NextAttemptAt),
		AttemptCount:      record.AttemptCount,
		ExpiresAt:         record.ExpiresAt.UTC(),
		LastErrorCode:     copyUploadTaskValue(record.LastErrorCode),
		LastErrorMessage:  copyUploadTaskValue(record.LastErrorMessage),
		CreatedAt:         record.CreatedAt.UTC(),
		UpdatedAt:         record.UpdatedAt.UTC(),
		CompletedAt:       copyUploadTaskValue(record.CompletedAt),
	}
	for _, stamp := range []*time.Time{result.LeaseUntil, result.NextAttemptAt, result.CompletedAt} {
		if stamp != nil {
			*stamp = stamp.UTC()
		}
	}

	// 6. 复查取消，返回独立领域记录。
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("查询上传任务已取消: %w", err)
	}
	return result, nil
}

// copyUploadTaskValue 为任务中的可空标量生成独立副本，nil 保留为空。
func copyUploadTaskValue[Value any](source *Value) *Value {
	// 1. 保留 NULL，或按值复制指针目标。
	if source == nil {
		return nil
	}
	value := *source
	return &value
}
