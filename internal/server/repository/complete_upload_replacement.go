package repository

import (
	"context"
	"fmt"
	"math"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm/clause"
)

// CompleteUploadReplacement 保存替换成功时的原文件 ID 和实际发布版本。
// 调用方已在同一事务保留旧内容、替换引用并结算配额，负责传播错误及确认提交。
func (store *Store) CompleteUploadReplacement(ctx context.Context, task *domain.UploadTask, objectID uint64, entry *domain.FileEntry) (*domain.UploadTask, error) {
	// 1. 复核事务、原任务绑定、结果文件及仍有效的校验租约。
	if err := store.validateUploadPublicationCompletionInput(ctx, task, objectID, entry); err != nil {
		return nil, err
	}
	if task.ReplacementEntryID == nil || *task.ReplacementEntryID == 0 ||
		task.ReplacementExpectedRevision == nil || *task.ReplacementExpectedRevision == 0 || task.ExpectedSHA256 == nil {
		return nil, domain.ErrInvalidUploadTaskData
	}
	expectedRevision := *task.ReplacementExpectedRevision
	if expectedRevision == math.MaxUint64 {
		return nil, domain.ErrEntryRevisionExhausted
	}
	if entry.ID != *task.ReplacementEntryID || entry.Revision != expectedRevision+1 || entry.FileSize != task.FileSize {
		return nil, domain.ErrInvalidFileEntryData
	}
	lockedStore := *store
	lockedStore.database = store.database.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"})
	current, err := lockedStore.findUploadTaskReadyForPublicationCompletion(ctx, task)
	if err != nil {
		return nil, err
	}
	if current.ReplacementEntryID == nil || *current.ReplacementEntryID != entry.ID ||
		current.ReplacementExpectedRevision == nil || *current.ReplacementExpectedRevision != expectedRevision ||
		current.ExpectedSHA256 == nil || *current.ExpectedSHA256 != *task.ExpectedSHA256 {
		return nil, domain.ErrInvalidUploadTaskData
	}

	// 2. 只有旧版本、当前文件和已核验对象均吻合时，才写完成快照并清除租约。
	result := store.database.WithContext(ctx).Exec(`UPDATE upload_tasks AS upload_task
		JOIN file_entries AS result_entry ON result_entry.user_id = upload_task.user_id AND result_entry.id = upload_task.replacement_entry_id
		JOIN file_versions AS history_version ON history_version.user_id = upload_task.user_id
			AND history_version.entry_id = result_entry.id AND history_version.revision = upload_task.replacement_expected_revision
		JOIN stored_objects AS published_object ON published_object.owner_user_id = upload_task.user_id AND published_object.id = result_entry.object_id
		SET upload_task.status = 'COMPLETED', upload_task.published_object_id = published_object.id,
			upload_task.result_entry_id = result_entry.id, upload_task.resolved_name = result_entry.name,
			upload_task.replacement_published_revision = result_entry.revision,
			upload_task.completed_at = UTC_TIMESTAMP(6), upload_task.updated_at = UTC_TIMESTAMP(6),
			upload_task.lease_owner = NULL, upload_task.lease_until = NULL, upload_task.next_attempt_at = NULL,
			upload_task.last_error_code = NULL, upload_task.last_error_message = NULL
		WHERE upload_task.user_id = ? AND upload_task.id = ? AND upload_task.staged_object_id = ?
			AND CAST(upload_task.lease_owner AS BINARY) = ? AND upload_task.lease_version = ?
			AND upload_task.lease_until > UTC_TIMESTAMP(6) AND upload_task.expires_at > UTC_TIMESTAMP(6)
			AND upload_task.status = 'VERIFYING' AND upload_task.upload_mode = ? AND upload_task.reserved_bytes = 0
			AND CAST(upload_task.storage_upload_id AS BINARY) <=> ?
			AND upload_task.replacement_entry_id = ? AND upload_task.replacement_expected_revision = ?
			AND upload_task.replacement_published_revision IS NULL AND upload_task.replacement_conflicting_revision IS NULL
			AND upload_task.published_object_id IS NULL AND upload_task.result_entry_id IS NULL
			AND upload_task.resolved_name IS NULL AND upload_task.completed_at IS NULL
			AND result_entry.parent_id = ? AND result_entry.entry_type = 'FILE' AND result_entry.status = 'ACTIVE'
			AND result_entry.revision = ? AND result_entry.content_modified_at IS NOT NULL AND CAST(result_entry.name AS BINARY) = ?
			AND result_entry.file_size = upload_task.file_size AND upload_task.file_size = ?
			AND published_object.id = ? AND published_object.status = 'READY' AND published_object.file_size = upload_task.file_size
			AND published_object.verified_sha256 = upload_task.expected_sha256`,
		current.UserID, current.ID, current.StagedObjectID, *current.LeaseOwner, current.LeaseVersion,
		current.UploadMode, current.StorageUploadID, entry.ID, expectedRevision, *entry.ParentID, entry.Revision, entry.Name, current.FileSize, objectID)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if result.Error != nil {
		return nil, fmt.Errorf("保存内容替换完成结果失败: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return nil, domain.ErrUploadTaskBusy
	}
	if result.RowsAffected != 1 {
		return nil, domain.ErrInvalidUploadTaskData
	}

	// 3. 读回提交所需的结果，核对公开版本及原绑定仍与本次替换一致。
	completed, err := lockedStore.readCompletedUploadPublication(ctx, current, objectID, entry)
	if err != nil {
		return nil, err
	}
	if completed.ReplacementEntryID == nil || *completed.ReplacementEntryID != entry.ID ||
		completed.ReplacementExpectedRevision == nil || *completed.ReplacementExpectedRevision != expectedRevision ||
		completed.ReplacementPublishedRevision == nil || *completed.ReplacementPublishedRevision != entry.Revision ||
		completed.ReplacementConflictingRevision != nil ||
		completed.ExpectedSHA256 == nil || *completed.ExpectedSHA256 != *current.ExpectedSHA256 ||
		completed.RequestedName != current.RequestedName || !completed.CreatedAt.Equal(current.CreatedAt) || !completed.ExpiresAt.Equal(current.ExpiresAt) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	return completed, nil
}
