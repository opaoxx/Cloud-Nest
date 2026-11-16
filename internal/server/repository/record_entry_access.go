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

// RecordUserEntryAccess 在已认证用户事务内记录活动文件的最近访问时间。
// 上层须在同一事务检查完整路径与访问授权，回调须传播失败。
// 六十秒内复用原访问时间，返回偏好由外层确认提交后交付。
func (store *Store) RecordUserEntryAccess(ctx context.Context, entryID uint64) (*domain.EntryPreference, error) {
	// 1. 检查已认证用户事务、目标 ID 和取消状态。
	if ctx == nil || entryID == 0 || store.authenticatedActor == nil || store.loginUserID == 0 {
		return nil, fmt.Errorf("记录访问需要上下文、非零目标 ID 和已认证用户事务")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return nil, fmt.Errorf("记录访问必须使用已认证事务 Store")
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
		return nil, fmt.Errorf("定位待记录访问的目录项失败: %w", queryError)
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
		return nil, fmt.Errorf("锁定待记录访问的目录项失败: %w", queryError)
	}

	// 3. 核对活动文件及基础结构。
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
	if target.EntryType != "FILE" {
		return nil, domain.ErrFileEntryNotFile
	}

	// 4. 首次记录或按六十秒窗口更新最近访问。
	// 更新时间先依据原访问时间判断，随后才设置新的访问时间。
	statement := "INSERT INTO user_entry_preferences (user_id, entry_id, last_opened_at) VALUES (?, ?, UTC_TIMESTAMP(6)) ON DUPLICATE KEY UPDATE updated_at = IF(last_opened_at IS NULL OR last_opened_at <= UTC_TIMESTAMP(6) - INTERVAL 60 SECOND, UTC_TIMESTAMP(6), updated_at), last_opened_at = IF(last_opened_at IS NULL OR last_opened_at <= UTC_TIMESTAMP(6) - INTERVAL 60 SECOND, UTC_TIMESTAMP(6), last_opened_at)"
	result := store.database.WithContext(writeContext).Exec(statement, actor.UserID, entryID)
	if err := writeContext.Err(); err != nil {
		return nil, err
	}
	if result.Error != nil {
		return nil, fmt.Errorf("记录目录项最近访问失败: %w", result.Error)
	}
	if result.RowsAffected < 0 || result.RowsAffected > 2 {
		return nil, domain.ErrInvalidFileEntryData
	}

	// 5. 当前读取写入后的偏好。
	var preference models.EntryPreference
	queryError = store.database.WithContext(writeContext).Table("user_entry_preferences").
		Select("user_id", "entry_id", "starred_at", "last_opened_at").
		Clauses(clause.Locking{Strength: "UPDATE"}).Where("user_id = ? AND entry_id = ?", actor.UserID, entryID).Take(&preference).Error
	if err := writeContext.Err(); err != nil {
		return nil, err
	}
	if errors.Is(queryError, gorm.ErrRecordNotFound) {
		return nil, domain.ErrInvalidFileEntryData
	}
	if queryError != nil {
		return nil, fmt.Errorf("读取记录访问后的偏好失败: %w", queryError)
	}

	// 6. 核对结果并复制可空时间。
	if preference.UserID != actor.UserID || preference.EntryID != entryID || preference.LastOpenedAt == nil {
		return nil, domain.ErrInvalidFileEntryData
	}
	for _, stamp := range []*time.Time{preference.StarredAt, preference.LastOpenedAt} {
		if stamp != nil && (stamp.IsZero() || stamp.UTC().Year() < 1000 || stamp.UTC().Year() > 9999) {
			return nil, domain.ErrInvalidFileEntryData
		}
	}
	openedAt := *preference.LastOpenedAt
	preferenceCopy := domain.EntryPreference{UserID: preference.UserID, EntryID: preference.EntryID, LastOpenedAt: &openedAt}
	if preference.StarredAt != nil {
		starredAt := *preference.StarredAt
		preferenceCopy.StarredAt = &starredAt
	}
	return &preferenceCopy, nil
}
