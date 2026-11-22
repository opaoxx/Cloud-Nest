package repository

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm/clause"
)

// ReplaceUploadFileContent 在所属账号事务内把原文件改指向校验通过的对象。
// 调用方须在同一事务保存旧引用、保护新对象并结算任务、配额及事件，任何失败均须回滚。
func (store *Store) ReplaceUploadFileContent(
	ctx context.Context, entryID, expectedRevision, objectID uint64, digest domain.UploadContentDigest,
) (*domain.FileEntry, error) {
	// 1. 核对输入及当前文件，要求原版本和可见路径仍有效。
	if objectID == 0 {
		return nil, domain.ErrInvalidStoredObjectData
	}
	var mimeType *string
	if digest.DetectedMIMEType != "" {
		if !domain.ValidDownloadMediaType(digest.DetectedMIMEType) {
			return nil, domain.ErrInvalidFileEntryData
		}
		mimeType = &digest.DetectedMIMEType
	}
	current, err := store.LockUploadReplacementTarget(ctx, entryID, expectedRevision)
	if err != nil {
		return nil, err
	}

	// 2. 在同一事务锁读发布对象，核对账号、状态及本次校验的大小和摘要。
	lockedObjectStore := *store
	lockedObjectStore.database = store.database.WithContext(ctx).
		Table("stored_objects FORCE INDEX (uk_objects_owner_id)").
		Clauses(clause.Locking{Strength: "UPDATE"})
	object, err := lockedObjectStore.FindUserStoredObject(ctx, current.UserID, objectID)
	if err != nil {
		return nil, err
	}
	if object.Status != domain.StoredObjectStatusReady || object.FileSize != digest.SizeBytes ||
		object.VerifiedSHA256 == nil || *object.VerifiedSHA256 != digest.SHA256 {
		return nil, domain.ErrInvalidStoredObjectData
	}

	// 3. 用同一数据库时间写入内容、大小和修改时间，按原版本条件递增一次。
	modifiedAt, err := store.readPublishedFileEntryTime(ctx)
	if err != nil {
		return nil, err
	}
	result := store.database.WithContext(ctx).Exec(`UPDATE file_entries
		SET object_id=?,file_size=?,mime_type=?,content_modified_at=?,modified_at=?,updated_at=?,revision=revision+1
		WHERE user_id=? AND id=? AND status='ACTIVE' AND entry_type='FILE' AND revision=?`,
		object.ID, object.FileSize, mimeType, modifiedAt, modifiedAt, modifiedAt, current.UserID, current.ID, expectedRevision)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if result.Error != nil {
		return nil, fmt.Errorf("更新文件内容引用失败: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return nil, domain.ErrInvalidFileEntryData
	}

	// 4. 读回条目、内容和发布时间，确认原身份保留且更新值准确。
	lockedEntryStore := store.lockedFileEntryStore(ctx)
	updated, err := lockedEntryStore.FindUserFileEntry(ctx, current.UserID, current.ID)
	if err != nil {
		return nil, err
	}
	content, err := lockedEntryStore.FindUserFileEntryContent(ctx, current.UserID, current.ID)
	if err != nil {
		return nil, err
	}
	display, err := lockedEntryStore.FindUserFileEntryDisplay(ctx, current.UserID, current.ID)
	if err != nil {
		return nil, err
	}
	if updated.ParentID == nil || *updated.ParentID != *current.ParentID || updated.Name != current.Name ||
		updated.EntryType != "FILE" || updated.Status != "ACTIVE" || !updated.CreatedAt.Equal(current.CreatedAt) ||
		updated.FileSize != digest.SizeBytes || updated.Revision != expectedRevision+1 ||
		!updated.ModifiedAt.Equal(modifiedAt) || !updated.UpdatedAt.Equal(modifiedAt) ||
		content.ObjectID == nil || *content.ObjectID != objectID || !sameUploadStorageText(content.MIMEType, mimeType) ||
		display.ContentModifiedAt == nil || !display.ContentModifiedAt.Equal(modifiedAt) {
		return nil, domain.ErrInvalidFileEntryData
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return updated, nil
}
