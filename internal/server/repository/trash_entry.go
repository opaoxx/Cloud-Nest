package repository

import (
	"context"
	"errors"
	"fmt"
	"math"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// TrashUserFileEntry 在已认证用户事务内按预期版本回收普通目录项。
// 调用方先在同一事务检查完整活动路径，回调须将错误交给外层处理。
func (store *Store) TrashUserFileEntry(ctx context.Context, entryID, expectedRevision uint64) (*domain.TrashedFileEntry, error) {
	// 1. 校验事务身份，按用户范围定位并锁定目录项。
	if ctx == nil || entryID == 0 || expectedRevision == 0 || store == nil || store.database == nil ||
		store.authenticatedActor == nil || store.loginUserID == 0 {
		return nil, fmt.Errorf("回收目录项需要上下文、非零目标和预期版本以及已认证用户事务")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return nil, fmt.Errorf("回收目录项必须使用已认证事务 Store")
	}
	actor := *store.authenticatedActor
	if actor.UserID != store.loginUserID || actor.DeviceID == 0 || actor.SessionID == 0 {
		return nil, domain.ErrInvalidSessionData
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var scope models.FileEntry
	queryError := store.database.WithContext(ctx).Select("id", "user_id").
		Where("user_id = ? AND id = ?", actor.UserID, entryID).Take(&scope).Error
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if errors.Is(queryError, gorm.ErrRecordNotFound) {
		return nil, domain.ErrFileEntryNotFound
	}
	if queryError != nil {
		return nil, fmt.Errorf("定位待回收目录项失败: %w", queryError)
	}
	if scope.ID != entryID || scope.UserID != actor.UserID {
		return nil, domain.ErrInvalidFileEntryData
	}
	current, err := store.readLockedTrashEntry(ctx, actor.UserID, entryID)
	if err != nil {
		return nil, err
	}

	// 2. 核对活动状态、根目录限制与当前版本。
	if current.Status != "ACTIVE" {
		return nil, domain.ErrFileEntryNotFound
	}
	if current.Revision == 0 || current.DeletedAt != nil || current.CreatedAt.IsZero() ||
		(current.EntryType != "FILE" && current.EntryType != "DIR") || (current.EntryType == "DIR" && current.FileSize != 0) {
		return nil, domain.ErrInvalidFileEntryData
	}
	if current.ParentID == nil {
		if current.EntryType != "DIR" || current.Name != "" {
			return nil, domain.ErrInvalidFileEntryData
		}
		return nil, domain.ErrRootEntryTrash
	}
	if *current.ParentID == 0 || *current.ParentID == entryID || current.Name == "" || !utf8.ValidString(current.Name) {
		return nil, domain.ErrInvalidFileEntryData
	}
	if current.Revision != expectedRevision {
		return nil, &domain.EntryRevisionConflictError{CurrentRevision: current.Revision}
	}
	if current.Revision == math.MaxUint64 {
		return nil, domain.ErrEntryRevisionExhausted
	}

	// 3. 用数据库时间更新回收状态，读回并核对结果。
	result := store.database.WithContext(ctx).Exec(`
		UPDATE file_entries
		SET status = 'TRASHED', deleted_at = UTC_TIMESTAMP(6), revision = revision + 1,
		    modified_at = UTC_TIMESTAMP(6), updated_at = UTC_TIMESTAMP(6)
		WHERE user_id = ? AND id = ? AND parent_id = ? AND status = 'ACTIVE' AND revision = ?`,
		actor.UserID, entryID, *current.ParentID, expectedRevision)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if result.Error != nil {
		return nil, fmt.Errorf("写入目录项回收状态失败: %w", result.Error)
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
	if saved.Status != "TRASHED" || saved.Revision != expectedRevision+1 || saved.DeletedAt == nil ||
		saved.ParentID == nil || *saved.ParentID != *current.ParentID || saved.Name != current.Name ||
		saved.EntryType != current.EntryType || saved.FileSize != current.FileSize || !saved.CreatedAt.Equal(current.CreatedAt) {
		return nil, domain.ErrInvalidFileEntryData
	}
	deletedAt := saved.DeletedAt.UTC()
	if deletedAt.Year() < 1000 || deletedAt.Year() > 9999 || deletedAt.Nanosecond()%1000 != 0 ||
		!saved.ModifiedAt.Equal(deletedAt) || !saved.UpdatedAt.Equal(deletedAt) {
		return nil, domain.ErrInvalidFileEntryData
	}
	parentID := *saved.ParentID
	return &domain.TrashedFileEntry{
		FileEntry: domain.FileEntry{
			ID: saved.ID, UserID: saved.UserID, ParentID: &parentID, Name: saved.Name, EntryType: saved.EntryType,
			FileSize: saved.FileSize, Revision: saved.Revision, Status: saved.Status,
			CreatedAt: saved.CreatedAt, ModifiedAt: saved.ModifiedAt, UpdatedAt: saved.UpdatedAt,
		},
		DeletedAt: deletedAt,
	}, nil
}

// readLockedTrashEntry 读取当前事务锁住的目录项及其回收时间。
func (store *Store) readLockedTrashEntry(ctx context.Context, userID, entryID uint64) (*models.FileEntryTrash, error) {
	// 1. 按用户和主键当前读，获取或继续持有目标行锁。
	var entry models.FileEntryTrash
	queryError := store.database.WithContext(ctx).
		Select("id", "user_id", "parent_id", "name", "entry_type", "file_size", "revision", "status", "created_at", "modified_at", "updated_at", "deleted_at").
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("user_id = ? AND id = ?", userID, entryID).Take(&entry).Error
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if errors.Is(queryError, gorm.ErrRecordNotFound) {
		return nil, domain.ErrFileEntryNotFound
	}
	if queryError != nil {
		return nil, fmt.Errorf("读取已锁定的回收目录项失败: %w", queryError)
	}

	// 2. 复核归属及主键后交给回收流程。
	if entry.UserID != userID || entry.ID != entryID {
		return nil, domain.ErrInvalidFileEntryData
	}
	return &entry, nil
}
