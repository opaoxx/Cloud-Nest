package repository

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// SetUserEntryStarred 在已认证用户事务内设置目录项收藏状态。
// 完整祖先可见性由上层在同一用户事务检查，回调须向外返回错误。
// 返回偏好在外层确认提交后生效，重复收藏沿用原收藏时间。
func (store *Store) SetUserEntryStarred(ctx context.Context, entryID uint64, starred bool) (*domain.EntryPreference, error) {
	// 1. 检查已认证用户事务、目标 ID 和取消状态。
	if ctx == nil || entryID == 0 || store.authenticatedActor == nil || store.loginUserID == 0 {
		return nil, fmt.Errorf("设置收藏需要上下文、非零目标 ID 和已认证用户事务")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return nil, fmt.Errorf("设置收藏必须使用已认证事务 Store")
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

	// 2. 按用户范围定位并锁定当前目录项。
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
		return nil, fmt.Errorf("定位待设置收藏的目录项失败: %w", queryError)
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
		return nil, fmt.Errorf("锁定待设置收藏的目录项失败: %w", queryError)
	}

	// 3. 核对活动状态及文件、目录结构。
	if target.ID != entryID || target.UserID != actor.UserID || target.Revision == 0 || target.CreatedAt.IsZero() || target.ModifiedAt.IsZero() || target.UpdatedAt.IsZero() {
		return nil, domain.ErrInvalidFileEntryData
	}
	if target.Status != "ACTIVE" {
		return nil, domain.ErrFileEntryNotFound
	}
	if (target.EntryType != "FILE" && target.EntryType != "DIR") || (target.EntryType == "DIR" && target.FileSize != 0) || !utf8.ValidString(target.Name) {
		return nil, domain.ErrInvalidFileEntryData
	}
	if target.ParentID == nil {
		if target.EntryType != "DIR" || target.Name != "" {
			return nil, domain.ErrInvalidFileEntryData
		}
	} else if *target.ParentID == 0 || *target.ParentID == target.ID || target.Name == "" {
		return nil, domain.ErrInvalidFileEntryData
	}

	// 4. 按目标状态写入收藏偏好。
	statement := "UPDATE user_entry_preferences SET starred_at = NULL, updated_at = UTC_TIMESTAMP(6) WHERE user_id = ? AND entry_id = ? AND starred_at IS NOT NULL"
	if starred {
		// 先用原收藏状态判断更新时间，再填充首次收藏时间。
		statement = "INSERT INTO user_entry_preferences (user_id, entry_id, starred_at) VALUES (?, ?, UTC_TIMESTAMP(6)) ON DUPLICATE KEY UPDATE updated_at = IF(starred_at IS NULL, UTC_TIMESTAMP(6), updated_at), starred_at = COALESCE(starred_at, UTC_TIMESTAMP(6))"
	}
	result := store.database.WithContext(writeContext).Exec(statement, actor.UserID, entryID)
	if err := writeContext.Err(); err != nil {
		return nil, err
	}
	if result.Error != nil {
		return nil, fmt.Errorf("设置目录项收藏状态失败: %w", result.Error)
	}
	maximumAffected := int64(1)
	if starred {
		maximumAffected = 2
	}
	if result.RowsAffected < 0 || result.RowsAffected > maximumAffected {
		return nil, domain.ErrInvalidFileEntryData
	}

	// 5. 当前读取偏好，处理缺失记录。
	var preference models.EntryPreference
	queryError = store.database.WithContext(writeContext).Table("user_entry_preferences").
		Select("user_id", "entry_id", "starred_at", "last_opened_at").
		Clauses(clause.Locking{Strength: "UPDATE"}).Where("user_id = ? AND entry_id = ?", actor.UserID, entryID).Take(&preference).Error
	if err := writeContext.Err(); err != nil {
		return nil, err
	}
	if errors.Is(queryError, gorm.ErrRecordNotFound) {
		if starred || result.RowsAffected != 0 {
			return nil, domain.ErrInvalidFileEntryData
		}
		return &domain.EntryPreference{UserID: actor.UserID, EntryID: entryID}, nil
	}
	if queryError != nil {
		return nil, fmt.Errorf("读取设置后的目录项偏好失败: %w", queryError)
	}

	// 6. 核对结果并复制时间字段。
	if preference.UserID != actor.UserID || preference.EntryID != entryID || (preference.StarredAt != nil) != starred {
		return nil, domain.ErrInvalidFileEntryData
	}
	for _, stamp := range []*time.Time{preference.StarredAt, preference.LastOpenedAt} {
		if stamp != nil && (stamp.IsZero() || stamp.UTC().Year() < 1000 || stamp.UTC().Year() > 9999) {
			return nil, domain.ErrInvalidFileEntryData
		}
	}
	preferenceCopy := domain.EntryPreference{UserID: preference.UserID, EntryID: preference.EntryID}
	if preference.StarredAt != nil {
		starredAt := *preference.StarredAt
		preferenceCopy.StarredAt = &starredAt
	}
	if preference.LastOpenedAt != nil {
		openedAt := *preference.LastOpenedAt
		preferenceCopy.LastOpenedAt = &openedAt
	}
	return &preferenceCopy, nil
}
