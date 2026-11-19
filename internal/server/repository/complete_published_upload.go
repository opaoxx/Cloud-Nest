package repository

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// CompletePublishedUpload 保存新建文件的发布结果，完成已结算配额的上传任务。
// 调用方在同一写回复核事务中创建文件、结算配额，并负责传播错误和确认提交。
func (store *Store) CompletePublishedUpload(ctx context.Context, task *domain.UploadTask, objectID uint64, entry *domain.FileEntry) (*domain.UploadTask, error) {
	// 1. 核对当前事务、租约和已结算的任务。
	if store == nil || store.database == nil || store.loginUserID == 0 || ctx == nil {
		return nil, fmt.Errorf("完成上传发布需要上下文和已锁定用户的事务")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return nil, fmt.Errorf("完成上传发布必须使用事务 Store")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	userID := store.loginUserID
	if task == nil || task.ID == 0 || task.UserID != userID || task.StagedObjectID == 0 ||
		task.LeaseVersion == 0 || task.LeaseOwner == nil || *task.LeaseOwner == "" || task.LeaseUntil == nil {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if objectID == 0 {
		return nil, domain.ErrInvalidStoredObjectData
	}
	if entry == nil || entry.ID == 0 || entry.UserID != userID || entry.ParentID == nil ||
		entry.EntryType != "FILE" || entry.Status != "ACTIVE" || entry.Revision != 1 ||
		entry.Name == "" || !utf8.ValidString(entry.Name) {
		return nil, domain.ErrInvalidFileEntryData
	}
	lockedStore := *store
	lockedStore.database = store.database.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"})
	current, err := lockedStore.FindUserUploadTask(ctx, userID, task.ID)
	if err != nil {
		return nil, err
	}
	if current.Status != domain.UploadStatusVerifying || current.UploadMode != "SINGLE" {
		return nil, domain.ErrUploadTaskNotReady
	}
	if current.StagedObjectID != task.StagedObjectID || current.LeaseVersion != task.LeaseVersion ||
		current.LeaseOwner == nil || *current.LeaseOwner != *task.LeaseOwner || current.LeaseUntil == nil {
		return nil, domain.ErrUploadTaskBusy
	}
	if current.FileSize != task.FileSize || current.ReservedBytes != 0 || current.PublishedObjectID != nil ||
		current.ResultEntryID != nil || current.ResolvedName != nil || current.CompletedAt != nil {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if current.FileSize > domain.MaxSingleUploadSizeBytes {
		return nil, domain.ErrUploadFileTooLarge
	}
	if *entry.ParentID != current.TargetParentID || entry.FileSize != current.FileSize {
		return nil, domain.ErrInvalidFileEntryData
	}

	// 2. 检查新文件与发布对象的对应关系，条件写入完成结果。
	result := store.database.WithContext(ctx).Exec(
		"UPDATE upload_tasks AS upload_task "+
			"JOIN file_entries AS result_entry ON result_entry.user_id = upload_task.user_id AND result_entry.id = ? "+
			"JOIN stored_objects AS published_object ON published_object.owner_user_id = upload_task.user_id AND published_object.id = result_entry.object_id "+
			"SET upload_task.status = 'COMPLETED', upload_task.published_object_id = published_object.id, "+
			"upload_task.result_entry_id = result_entry.id, upload_task.resolved_name = result_entry.name, "+
			"upload_task.completed_at = UTC_TIMESTAMP(6), upload_task.updated_at = UTC_TIMESTAMP(6), "+
			"upload_task.lease_owner = NULL, upload_task.lease_until = NULL, upload_task.next_attempt_at = NULL, "+
			"upload_task.last_error_code = NULL, upload_task.last_error_message = NULL "+
			"WHERE upload_task.user_id = ? AND upload_task.id = ? AND upload_task.staged_object_id = ? "+
			"AND CAST(upload_task.lease_owner AS BINARY) = ? AND upload_task.lease_version = ? "+
			"AND upload_task.lease_until > UTC_TIMESTAMP(6) AND upload_task.expires_at > UTC_TIMESTAMP(6) "+
			"AND upload_task.status = 'VERIFYING' AND upload_task.upload_mode = 'SINGLE' AND upload_task.reserved_bytes = 0 "+
			"AND upload_task.published_object_id IS NULL AND upload_task.result_entry_id IS NULL "+
			"AND upload_task.resolved_name IS NULL AND upload_task.completed_at IS NULL "+
			"AND result_entry.parent_id = upload_task.target_parent_id AND result_entry.source_device_id = upload_task.device_id "+
			"AND result_entry.entry_type = 'FILE' AND result_entry.status = 'ACTIVE' AND result_entry.revision = 1 "+
			"AND result_entry.content_modified_at IS NOT NULL AND CAST(result_entry.name AS BINARY) = ? "+
			"AND result_entry.file_size = upload_task.file_size AND upload_task.file_size = ? "+
			"AND published_object.id = ? AND published_object.status = 'READY' AND published_object.file_size = upload_task.file_size "+
			"AND (upload_task.expected_sha256 IS NULL OR upload_task.expected_sha256 = published_object.verified_sha256)",
		entry.ID, userID, current.ID, current.StagedObjectID, *current.LeaseOwner, current.LeaseVersion, entry.Name, current.FileSize, objectID)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if result.Error != nil {
		return nil, fmt.Errorf("写入上传完成结果失败: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return nil, domain.ErrUploadTaskBusy
	}
	if result.RowsAffected != 1 {
		return nil, domain.ErrInvalidUploadTaskData
	}

	// 3. 读回完成任务，核对结果、完成时间和租约释放。
	completed, err := lockedStore.FindUserUploadTask(ctx, userID, current.ID)
	if errors.Is(err, domain.ErrUploadTaskNotFound) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if err != nil {
		return nil, err
	}
	if completed.Status != domain.UploadStatusCompleted || completed.PublishedObjectID == nil || *completed.PublishedObjectID != objectID ||
		completed.ResultEntryID == nil || *completed.ResultEntryID != entry.ID ||
		completed.ResolvedName == nil || *completed.ResolvedName != entry.Name ||
		completed.CompletedAt == nil || !completed.CompletedAt.Equal(completed.UpdatedAt) ||
		!completed.CompletedAt.Before(current.ExpiresAt) || !completed.CompletedAt.Before(*current.LeaseUntil) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if completed.LeaseOwner != nil || completed.LeaseUntil != nil || completed.LeaseVersion != current.LeaseVersion ||
		completed.ReservedBytes != 0 || completed.LastErrorCode != nil || completed.LastErrorMessage != nil || completed.NextAttemptAt != nil ||
		completed.StagedObjectID != current.StagedObjectID || completed.TargetParentID != current.TargetParentID ||
		completed.DeviceID != current.DeviceID || completed.FileSize != current.FileSize || completed.AttemptCount != current.AttemptCount {
		return nil, domain.ErrInvalidUploadTaskData
	}
	return completed, nil
}
