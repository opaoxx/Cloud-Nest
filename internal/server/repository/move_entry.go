package repository

import (
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

// MoveUserFileEntry 在已认证用户事务内按预期版本移动普通目录项。
// 上层在同一事务先完成完整路径、子树关系、规模和最终深度检查。
// 回调须向外返回错误，返回记录在外层事务确认提交后生效。
func (store *Store) MoveUserFileEntry(ctx context.Context, entryID, expectedRevision, targetParentID uint64) (*domain.FileEntry, error) {
	// 1. 检查已认证用户事务、输入和取消状态。
	if ctx == nil || entryID == 0 || expectedRevision == 0 || targetParentID == 0 || store.authenticatedActor == nil || store.loginUserID == 0 {
		return nil, fmt.Errorf("移动目录项需要上下文、非零源/预期版本/目标父目录和已认证用户事务")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return nil, fmt.Errorf("移动目录项必须使用已认证事务 Store")
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

	// 2. 按用户范围定位记录，再按主键升序锁定当前事实。
	identifiers := []uint64{entryID}
	if targetParentID != entryID {
		identifiers = append(identifiers, targetParentID)
		if identifiers[0] > identifiers[1] {
			identifiers[0], identifiers[1] = identifiers[1], identifiers[0]
		}
	}
	for _, identifier := range identifiers {
		var scope models.FileEntry
		queryError := store.database.WithContext(writeContext).Select("id", "user_id").
			Where("user_id = ? AND id = ?", actor.UserID, identifier).Take(&scope).Error
		if err := writeContext.Err(); err != nil {
			return nil, err
		}
		if errors.Is(queryError, gorm.ErrRecordNotFound) {
			return nil, domain.ErrFileEntryNotFound
		}
		if queryError != nil {
			return nil, fmt.Errorf("定位移动目录项失败: %w", queryError)
		}
		if scope.ID != identifier || scope.UserID != actor.UserID {
			return nil, domain.ErrInvalidFileEntryData
		}
	}
	locked := make(map[uint64]models.FileEntry, len(identifiers))
	for _, identifier := range identifiers {
		var record models.FileEntry
		queryError := store.database.WithContext(writeContext).
			Select("id", "user_id", "parent_id", "name", "entry_type", "file_size", "revision", "status", "created_at", "modified_at", "updated_at").
			Clauses(clause.Locking{Strength: "UPDATE"}).Where("user_id = ? AND id = ?", actor.UserID, identifier).Take(&record).Error
		if err := writeContext.Err(); err != nil {
			return nil, err
		}
		if errors.Is(queryError, gorm.ErrRecordNotFound) {
			return nil, domain.ErrFileEntryNotFound
		}
		if queryError != nil {
			return nil, fmt.Errorf("锁定移动目录项失败: %w", queryError)
		}
		if record.ID != identifier || record.UserID != actor.UserID || record.Revision == 0 || record.CreatedAt.IsZero() || record.ModifiedAt.IsZero() || record.UpdatedAt.IsZero() {
			return nil, domain.ErrInvalidFileEntryData
		}
		if record.Status != "ACTIVE" {
			return nil, domain.ErrFileEntryNotFound
		}
		if (record.EntryType != "FILE" && record.EntryType != "DIR") || (record.EntryType == "DIR" && record.FileSize != 0) || !utf8.ValidString(record.Name) {
			return nil, domain.ErrInvalidFileEntryData
		}
		if record.ParentID == nil {
			if record.EntryType != "DIR" || record.Name != "" {
				return nil, domain.ErrInvalidFileEntryData
			}
		} else if *record.ParentID == 0 || *record.ParentID == record.ID || record.Name == "" {
			return nil, domain.ErrInvalidFileEntryData
		}
		locked[identifier] = record
	}

	// 3. 检查源、目标目录及预期版本。
	source, target := locked[entryID], locked[targetParentID]
	if source.ParentID == nil {
		return nil, domain.ErrRootEntryMove
	}
	if target.EntryType != "DIR" {
		return nil, domain.ErrFileEntryNotDirectory
	}
	if entryID == targetParentID {
		return nil, domain.ErrEntryMoveIntoSubtree
	}
	if source.Revision != expectedRevision {
		return nil, &domain.EntryRevisionConflictError{CurrentRevision: source.Revision}
	}

	// 4. 当前父目录匹配时返回事实，检查实际递增上限。
	if *source.ParentID == targetParentID {
		parentID := *source.ParentID
		return &domain.FileEntry{
			ID: source.ID, UserID: source.UserID, ParentID: &parentID, Name: source.Name, EntryType: source.EntryType,
			FileSize: source.FileSize, Revision: source.Revision, Status: source.Status,
			CreatedAt: source.CreatedAt, ModifiedAt: source.ModifiedAt, UpdatedAt: source.UpdatedAt,
		}, nil
	}
	if source.Revision == ^uint64(0) {
		return nil, domain.ErrEntryRevisionExhausted
	}

	// 5. 按用户、源、活动状态和版本条件更新位置。
	result := store.database.WithContext(writeContext).Exec(
		"UPDATE file_entries SET parent_id = ?, revision = revision + 1, modified_at = UTC_TIMESTAMP(6), updated_at = UTC_TIMESTAMP(6) WHERE user_id = ? AND id = ? AND status = 'ACTIVE' AND revision = ?",
		targetParentID, actor.UserID, entryID, expectedRevision)
	if err := writeContext.Err(); err != nil {
		return nil, err
	}
	if result.Error != nil {
		var databaseError *mysql.MySQLError
		if errors.As(result.Error, &databaseError) && databaseError.Number == 1062 {
			return nil, domain.ErrEntryNameConflict
		}
		return nil, fmt.Errorf("移动目录项失败: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return nil, domain.ErrInvalidFileEntryData
	}

	// 6. 读回并核对移动结果，交由外层确认提交。
	entry, readError := store.FindUserFileEntry(writeContext, actor.UserID, entryID)
	if err := writeContext.Err(); err != nil {
		return nil, err
	}
	if errors.Is(readError, domain.ErrFileEntryNotFound) {
		return nil, domain.ErrInvalidFileEntryData
	}
	if readError != nil {
		return nil, fmt.Errorf("读取移动后目录项失败: %w", readError)
	}
	if entry.ParentID == nil || *entry.ParentID != targetParentID || entry.Name != source.Name || entry.EntryType != source.EntryType || entry.FileSize != source.FileSize ||
		entry.Revision != expectedRevision+1 || entry.Status != "ACTIVE" || !entry.CreatedAt.Equal(source.CreatedAt) || entry.ModifiedAt.IsZero() || entry.UpdatedAt.IsZero() {
		return nil, domain.ErrInvalidFileEntryData
	}
	parentID := *entry.ParentID
	entry.ParentID = &parentID
	return entry, nil
}
