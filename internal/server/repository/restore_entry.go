package repository

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
	"gorm.io/gorm"
)

// RestoreUserFileEntry 在已认证用户事务内按预期版本恢复普通目录项。
// 调用方在同一事务检查原父链、选址和查重，并将返回的错误交给事务回调处理。
func (store *Store) RestoreUserFileEntry(ctx context.Context, entryID, expectedRevision, parentID uint64, name string, nameKey []byte) (*domain.FileEntry, error) {
	// 1. 核对事务身份和输入，按主键顺序锁定回收项与实际父目录。
	if ctx == nil || entryID == 0 || expectedRevision == 0 || parentID == 0 || store == nil || store.database == nil ||
		store.authenticatedActor == nil || store.loginUserID == 0 {
		return nil, fmt.Errorf("恢复目录项需要上下文、非零目标和预期版本、实际父目录及已认证用户事务")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return nil, fmt.Errorf("恢复目录项必须使用已认证事务 Store")
	}
	actor := *store.authenticatedActor
	if actor.UserID != store.loginUserID || actor.DeviceID == 0 || actor.SessionID == 0 {
		return nil, domain.ErrInvalidSessionData
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > 120 ||
		len(nameKey) == 0 || len(nameKey) > 768 || !utf8.Valid(nameKey) {
		return nil, domain.ErrInvalidEntryName
	}
	keyCopy := bytes.Clone(nameKey)

	identifiers := []uint64{entryID}
	if parentID != entryID {
		identifiers = append(identifiers, parentID)
		slices.Sort(identifiers)
	}
	// 先按归属定位两条记录，避免锁到其他用户的目录项。
	for _, identifier := range identifiers {
		var scope models.FileEntry
		queryError := store.database.WithContext(ctx).Select("id", "user_id").
			Where("user_id = ? AND id = ?", actor.UserID, identifier).Take(&scope).Error
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if errors.Is(queryError, gorm.ErrRecordNotFound) {
			return nil, domain.ErrFileEntryNotFound
		}
		if queryError != nil {
			return nil, fmt.Errorf("定位恢复所需目录项失败: %w", queryError)
		}
		if scope.ID != identifier || scope.UserID != actor.UserID {
			return nil, domain.ErrInvalidFileEntryData
		}
	}
	locked := make(map[uint64]*models.FileEntryTrash, len(identifiers))
	for _, identifier := range identifiers {
		entry, err := store.readLockedTrashEntry(ctx, actor.UserID, identifier)
		if err != nil {
			return nil, err
		}
		locked[identifier] = entry
	}

	// 2. 用锁定后的记录复查状态、父目录和预期版本。
	source, parent := locked[entryID], locked[parentID]
	if source.ParentID == nil {
		if source.EntryType != "DIR" || source.Name != "" || source.Status != "ACTIVE" || source.FileSize != 0 || source.DeletedAt != nil || source.Revision == 0 {
			return nil, domain.ErrInvalidFileEntryData
		}
		return nil, domain.ErrRootEntryRestore
	}
	switch source.Status {
	case "TRASHED":
	case "ACTIVE", "PURGING", "PURGED":
		return nil, domain.ErrFileEntryNotFound
	default:
		return nil, domain.ErrInvalidFileEntryData
	}
	if *source.ParentID == 0 || *source.ParentID == entryID || source.Name == "" || !utf8.ValidString(source.Name) ||
		source.Revision == 0 || source.DeletedAt == nil || source.DeletedAt.IsZero() || source.CreatedAt.IsZero() ||
		(source.EntryType != "FILE" && source.EntryType != "DIR") || (source.EntryType == "DIR" && source.FileSize != 0) {
		return nil, domain.ErrInvalidFileEntryData
	}
	if parentID == entryID || parent.Revision == 0 || parent.EntryType != "DIR" || parent.FileSize != 0 {
		return nil, domain.ErrInvalidFileEntryData
	}
	if parent.ParentID == nil {
		if parent.Name != "" || parent.Status != "ACTIVE" || parent.DeletedAt != nil {
			return nil, domain.ErrInvalidFileEntryData
		}
	} else {
		// 回退只能去真实根目录，其余情况必须使用原父目录。
		if parentID != *source.ParentID || *parent.ParentID == 0 || *parent.ParentID == parentID || *parent.ParentID == entryID ||
			parent.Name == "" || !utf8.ValidString(parent.Name) {
			return nil, domain.ErrInvalidFileEntryData
		}
		switch parent.Status {
		case "ACTIVE":
			if parent.DeletedAt != nil {
				return nil, domain.ErrInvalidFileEntryData
			}
		case "TRASHED", "PURGING", "PURGED":
			return nil, domain.ErrRestoreTargetUnavailable
		default:
			return nil, domain.ErrInvalidFileEntryData
		}
	}
	if source.Revision != expectedRevision {
		return nil, &domain.EntryRevisionConflictError{CurrentRevision: source.Revision}
	}
	if source.Revision == math.MaxUint64 {
		return nil, domain.ErrEntryRevisionExhausted
	}

	// 3. 条件更新为 ACTIVE，清除回收时间，递增版本并读回结果。
	result := store.database.WithContext(ctx).Exec(`
		UPDATE file_entries
		SET parent_id = ?, name = ?, name_key = ?, status = 'ACTIVE', deleted_at = NULL,
		    revision = revision + 1, modified_at = UTC_TIMESTAMP(6), updated_at = UTC_TIMESTAMP(6)
		WHERE user_id = ? AND id = ? AND parent_id = ? AND status = 'TRASHED' AND revision = ? AND deleted_at IS NOT NULL`,
		parentID, name, keyCopy, actor.UserID, entryID, *source.ParentID, expectedRevision)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if result.Error != nil {
		var databaseError *mysql.MySQLError
		if errors.As(result.Error, &databaseError) && databaseError.Number == 1062 {
			return nil, domain.ErrEntryNameConflict
		}
		return nil, fmt.Errorf("写入目录项恢复状态失败: %w", result.Error)
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
	if saved.Status != "ACTIVE" || saved.DeletedAt != nil || saved.Revision != expectedRevision+1 ||
		saved.ParentID == nil || *saved.ParentID != parentID || saved.Name != name ||
		saved.EntryType != source.EntryType || saved.FileSize != source.FileSize || !saved.CreatedAt.Equal(source.CreatedAt) ||
		saved.ModifiedAt.IsZero() || !saved.ModifiedAt.Equal(saved.UpdatedAt) || saved.ModifiedAt.Nanosecond()%1000 != 0 {
		return nil, domain.ErrInvalidFileEntryData
	}
	return &domain.FileEntry{
		ID: saved.ID, UserID: saved.UserID, ParentID: &parentID, Name: saved.Name, EntryType: saved.EntryType,
		FileSize: saved.FileSize, Revision: saved.Revision, Status: saved.Status,
		CreatedAt: saved.CreatedAt, ModifiedAt: saved.ModifiedAt, UpdatedAt: saved.UpdatedAt,
	}, nil
}
