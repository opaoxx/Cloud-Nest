package repository

import (
	"context"
	"errors"
	"fmt"
	"math"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
)

// BeginUserEntryPurge 在已认证用户事务内按预期版本开始清理。
// 调用方先在同一事务复查预检凭证和范围，遇到错误时由外层事务回滚。
func (store *Store) BeginUserEntryPurge(ctx context.Context, entryID, expectedRevision uint64) (*domain.FileEntry, error) {
	// 1. 检查事务身份，按用户归属定位并锁定目标。
	if ctx == nil || entryID == 0 || expectedRevision == 0 || store == nil || store.database == nil ||
		store.authenticatedActor == nil || store.loginUserID == 0 {
		return nil, fmt.Errorf("开始永久清理需要上下文、非零目标和预期版本以及已认证用户事务")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return nil, fmt.Errorf("开始永久清理必须使用已认证事务 Store")
	}
	actor := *store.authenticatedActor
	if actor.UserID != store.loginUserID || actor.DeviceID == 0 || actor.SessionID == 0 {
		return nil, domain.ErrInvalidSessionData
	}
	// 先用普通查询检查归属，避免锁住其他用户的记录。
	if _, err := store.FindUserFileEntry(ctx, actor.UserID, entryID); err != nil {
		return nil, err
	}
	current, err := store.readLockedTrashEntry(ctx, actor.UserID, entryID)
	if err != nil {
		return nil, err
	}

	// 2. 校验回收状态、根目录限制和当前版本。
	if current.ParentID == nil {
		if current.EntryType != "DIR" || current.Name != "" || current.Status != "ACTIVE" ||
			current.FileSize != 0 || current.DeletedAt != nil || current.Revision == 0 {
			return nil, domain.ErrInvalidFileEntryData
		}
		return nil, domain.ErrRootEntryPurge
	}
	switch current.Status {
	case "TRASHED":
	case "ACTIVE", "PURGING", "PURGED":
		return nil, domain.ErrFileEntryNotFound
	default:
		return nil, domain.ErrInvalidFileEntryData
	}
	if *current.ParentID == 0 || *current.ParentID == entryID || current.Name == "" || !utf8.ValidString(current.Name) ||
		current.Revision == 0 || current.DeletedAt == nil || current.DeletedAt.IsZero() || current.CreatedAt.IsZero() ||
		(current.EntryType != "FILE" && current.EntryType != "DIR") || (current.EntryType == "DIR" && current.FileSize != 0) {
		return nil, domain.ErrInvalidFileEntryData
	}
	if current.Revision != expectedRevision {
		return nil, &domain.EntryRevisionConflictError{CurrentRevision: current.Revision}
	}
	if current.Revision == math.MaxUint64 {
		return nil, domain.ErrEntryRevisionExhausted
	}

	// 3. 条件更新为 PURGING，读回并核对结果。
	result := store.database.WithContext(ctx).Exec(`
		UPDATE file_entries
		SET status = 'PURGING', purge_started_at = UTC_TIMESTAMP(6), revision = revision + 1,
		    modified_at = UTC_TIMESTAMP(6), updated_at = UTC_TIMESTAMP(6)
		WHERE user_id = ? AND id = ? AND parent_id = ? AND status = 'TRASHED' AND revision = ? AND deleted_at IS NOT NULL`,
		actor.UserID, entryID, *current.ParentID, expectedRevision)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if result.Error != nil {
		return nil, fmt.Errorf("写入永久清理开始状态失败: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return nil, domain.ErrInvalidFileEntryData
	}

	saved, err := store.readLockedTrashEntry(ctx, actor.UserID, entryID)
	if errors.Is(err, domain.ErrFileEntryNotFound) {
		return nil, domain.ErrInvalidFileEntryData
	}
	if err != nil {
		return nil, err
	}
	if saved.Status != "PURGING" || saved.Revision != expectedRevision+1 || saved.PurgeStartedAt == nil ||
		saved.DeletedAt == nil || !saved.DeletedAt.Equal(*current.DeletedAt) ||
		saved.ParentID == nil || *saved.ParentID != *current.ParentID || saved.Name != current.Name ||
		saved.EntryType != current.EntryType || saved.FileSize != current.FileSize || !saved.CreatedAt.Equal(current.CreatedAt) {
		return nil, domain.ErrInvalidFileEntryData
	}
	startedAt := saved.PurgeStartedAt.UTC()
	if startedAt.Year() < 1000 || startedAt.Year() > 9999 || startedAt.Nanosecond()%1000 != 0 ||
		!saved.ModifiedAt.Equal(startedAt) || !saved.UpdatedAt.Equal(startedAt) {
		return nil, domain.ErrInvalidFileEntryData
	}

	parentID := *saved.ParentID
	return &domain.FileEntry{
		ID: saved.ID, UserID: saved.UserID, ParentID: &parentID, Name: saved.Name, EntryType: saved.EntryType,
		FileSize: saved.FileSize, Revision: saved.Revision, Status: saved.Status,
		CreatedAt: saved.CreatedAt, ModifiedAt: startedAt, UpdatedAt: startedAt,
	}, nil
}
