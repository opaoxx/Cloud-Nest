package repository

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// RenameUserFileEntry 在已认证用户事务内按预期版本重命名普通目录项。
// 名称元数据和判重键由上层计算，祖先可见性由上层在同一用户事务中检查。
// 回调须向外返回错误，返回记录在外层事务提交成功后确认生效。
func (store *Store) RenameUserFileEntry(ctx context.Context, entryID, expectedRevision uint64, metadata domain.EntryNameMetadata, nameKey []byte) (*domain.FileEntry, error) {
	// 1. 检查已认证用户事务和输入，复制可变字段。
	if ctx == nil || entryID == 0 || expectedRevision == 0 || store.authenticatedActor == nil || store.loginUserID == 0 {
		return nil, fmt.Errorf("重命名目录项需要上下文、非零目标及预期版本和已认证用户事务")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return nil, fmt.Errorf("重命名目录项必须使用已认证事务 Store")
	}
	actor := *store.authenticatedActor
	if actor.UserID != store.loginUserID || actor.DeviceID == 0 || actor.SessionID == 0 {
		return nil, domain.ErrInvalidSessionData
	}
	if metadata.Name == "" || !utf8.ValidString(metadata.Name) || len(nameKey) == 0 || len(nameKey) > 768 {
		return nil, domain.ErrInvalidEntryName
	}
	keyCopy := bytes.Clone(nameKey)
	if metadata.Extension != nil {
		extension := *metadata.Extension
		if !utf8.ValidString(extension) || utf8.RuneCountInString(extension) > 32 {
			return nil, domain.ErrInvalidFileEntryData
		}
		metadata.Extension = &extension
	}
	if metadata.Category != nil {
		category := *metadata.Category
		metadata.Category = &category
	}
	writeContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := writeContext.Err(); err != nil {
		return nil, err
	}

	// 2. 按用户归属定位目标，再锁定并读取当前记录。
	var scope models.FileEntry
	queryError := store.database.WithContext(writeContext).Select("id", "user_id").
		Where("user_id = ? AND id = ?", actor.UserID, entryID).Take(&scope).Error
	if err := writeContext.Err(); err != nil {
		return nil, err
	}
	if errors.Is(queryError, gorm.ErrRecordNotFound) {
		return nil, domain.ErrFileEntryNotFound
	}
	if queryError != nil {
		return nil, fmt.Errorf("定位待改名目录项失败: %w", queryError)
	}
	if scope.ID != entryID || scope.UserID != actor.UserID {
		return nil, domain.ErrInvalidFileEntryData
	}
	var target models.FileEntry
	queryError = store.database.WithContext(writeContext).
		Select("id", "user_id", "parent_id", "name", "entry_type", "file_size", "revision", "status", "created_at", "modified_at", "updated_at").
		Clauses(clause.Locking{Strength: "UPDATE"}).Where("user_id = ? AND id = ?", actor.UserID, entryID).Take(&target).Error
	if err := writeContext.Err(); err != nil {
		return nil, err
	}
	if errors.Is(queryError, gorm.ErrRecordNotFound) {
		return nil, domain.ErrFileEntryNotFound
	}
	if queryError != nil {
		return nil, fmt.Errorf("锁定待改名目录项失败: %w", queryError)
	}

	// 3. 检查活动状态、普通目录项规则及预期版本。
	if target.ID != entryID || target.UserID != actor.UserID || target.Revision == 0 || target.CreatedAt.IsZero() || target.ModifiedAt.IsZero() || target.UpdatedAt.IsZero() {
		return nil, domain.ErrInvalidFileEntryData
	}
	if target.Status != "ACTIVE" {
		return nil, domain.ErrFileEntryNotFound
	}
	if target.ParentID == nil {
		if target.EntryType != "DIR" || target.Name != "" || target.FileSize != 0 {
			return nil, domain.ErrInvalidFileEntryData
		}
		return nil, domain.ErrRootEntryRename
	}
	if *target.ParentID == 0 || *target.ParentID == target.ID || target.Name == "" || !utf8.ValidString(target.Name) {
		return nil, domain.ErrInvalidFileEntryData
	}
	switch target.EntryType {
	case "DIR":
		if target.FileSize != 0 || metadata.Extension != nil || metadata.Category != nil {
			return nil, domain.ErrInvalidFileEntryData
		}
	case "FILE":
		if metadata.Category == nil {
			return nil, domain.ErrInvalidFileEntryData
		}
		switch *metadata.Category {
		case "DOCUMENT", "IMAGE", "VIDEO", "AUDIO", "ARCHIVE", "OTHER":
		default:
			return nil, domain.ErrInvalidFileEntryData
		}
	default:
		return nil, domain.ErrInvalidFileEntryData
	}
	if target.Revision != expectedRevision {
		return nil, &domain.EntryRevisionConflictError{CurrentRevision: target.Revision}
	}

	// 4. 名称相同时返回当前事实，检查实际更新的版本上限。
	if target.Name == metadata.Name {
		parentID := *target.ParentID
		return &domain.FileEntry{
			ID: target.ID, UserID: target.UserID, ParentID: &parentID, Name: target.Name, EntryType: target.EntryType,
			FileSize: target.FileSize, Revision: target.Revision, Status: target.Status,
			CreatedAt: target.CreatedAt, ModifiedAt: target.ModifiedAt, UpdatedAt: target.UpdatedAt,
		}, nil
	}
	if target.Revision == ^uint64(0) {
		return nil, domain.ErrEntryRevisionExhausted
	}

	// 5. 按用户、目标、活动状态和版本条件更新名称元数据。
	result := store.database.WithContext(writeContext).Exec(
		"UPDATE file_entries SET name = ?, name_key = ?, extension = ?, category = ?, revision = revision + 1, modified_at = UTC_TIMESTAMP(6), updated_at = UTC_TIMESTAMP(6) WHERE user_id = ? AND id = ? AND status = 'ACTIVE' AND revision = ?",
		metadata.Name, keyCopy, metadata.Extension, metadata.Category, actor.UserID, entryID, expectedRevision)
	if err := writeContext.Err(); err != nil {
		return nil, err
	}
	if result.Error != nil {
		var databaseError *mysql.MySQLError
		if errors.As(result.Error, &databaseError) && databaseError.Number == 1062 {
			return nil, domain.ErrEntryNameConflict
		}
		return nil, fmt.Errorf("重命名目录项失败: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return nil, domain.ErrInvalidFileEntryData
	}

	// 6. 读回并核对更新结果，交由外层事务确认提交。
	entry, err := store.FindUserFileEntry(writeContext, actor.UserID, entryID)
	if errors.Is(err, domain.ErrFileEntryNotFound) {
		return nil, domain.ErrInvalidFileEntryData
	}
	if err != nil {
		return nil, fmt.Errorf("读取改名后目录项失败: %w", err)
	}
	if entry.Name != metadata.Name || entry.EntryType != target.EntryType || entry.FileSize != target.FileSize || entry.Revision != expectedRevision+1 || entry.Status != "ACTIVE" ||
		entry.ParentID == nil || *entry.ParentID != *target.ParentID || !entry.CreatedAt.Equal(target.CreatedAt) || entry.ModifiedAt.IsZero() || entry.UpdatedAt.IsZero() {
		return nil, domain.ErrInvalidFileEntryData
	}
	parentID := *entry.ParentID
	entry.ParentID = &parentID
	return entry, nil
}
