package repository

import (
	"context"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
)

// ListUserRecentEntryCandidates 按认证用户读取最近访问过的活动普通文件候选。
// 当前接收 recent/last_opened_desc、空搜索及空分类，before 为 nil 时读取首页。
// 调用方在同一只读快照内检查完整祖先链，再决定交付记录及最后扫描位置。
func (store *Store) ListUserRecentEntryCandidates(ctx context.Context, userID uint64, query domain.GlobalEntryQuery, before *domain.RecentEntryPosition) (*domain.RecentEntryPage, error) {
	// 1. 校验用户、查询条件、分页位置与上下文，设置查询期限。
	if ctx == nil || userID == 0 || !query.IsValid() || query.Scope != domain.GlobalEntryScopeRecent ||
		query.Sort != domain.GlobalEntrySortLastOpenedDesc || query.Search != "" || query.Kind != "" {
		return nil, fmt.Errorf("读取最近访问候选需要上下文、非零用户及 recent/last_opened_desc 的空筛选查询")
	}
	var upper *domain.RecentEntryPosition
	if before != nil {
		position := *before
		if !position.IsValid() {
			return nil, fmt.Errorf("最近访问候选翻页位置需要非零 ID 和数据库微秒时间")
		}
		position.LastOpenedAt = position.LastOpenedAt.UTC()
		upper = &position
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("读取最近访问候选失败: %w", err)
	}

	// 2. 关联当前用户偏好，读取活动文件及额外一条候选。
	statement := store.database.WithContext(queryContext).Table("file_entries").
		Select("file_entries.id", "file_entries.user_id", "file_entries.parent_id", "file_entries.name", "file_entries.entry_type", "file_entries.file_size", "file_entries.revision", "file_entries.status", "file_entries.created_at", "file_entries.modified_at", "file_entries.updated_at", "preferences.last_opened_at").
		Joins("JOIN user_entry_preferences AS preferences ON preferences.user_id = file_entries.user_id AND preferences.entry_id = file_entries.id").
		Where("file_entries.user_id = ? AND file_entries.status = ? AND file_entries.entry_type = ? AND file_entries.parent_id IS NOT NULL AND preferences.last_opened_at IS NOT NULL", userID, "ACTIVE", "FILE")
	if upper != nil {
		statement = statement.Where("(preferences.last_opened_at < ? OR (preferences.last_opened_at = ? AND file_entries.id < ?))", upper.LastOpenedAt, upper.LastOpenedAt, upper.ID)
	}
	var records []struct {
		models.FileEntry
		LastOpenedAt *time.Time `gorm:"column:last_opened_at;->"`
	}
	queryError := statement.Order("preferences.last_opened_at DESC, file_entries.id DESC").Limit(query.PageSize + 1).Find(&records).Error

	// 3. 处理取消、扫描及数据库错误。
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("读取最近访问候选已取消: %w", err)
	}
	if queryError != nil {
		return nil, fmt.Errorf("读取最近访问候选失败: %w", queryError)
	}

	// 4. 检查访问时间、文件结构和降序位置，复制候选。
	if len(records) > query.PageSize+1 {
		return nil, domain.ErrInvalidFileEntryData
	}
	entries := make([]domain.RecentEntryCandidate, 0, len(records))
	for _, record := range records {
		modifiedPosition := domain.FileEntryPosition{ModifiedAt: record.ModifiedAt, ID: record.ID}
		if record.LastOpenedAt == nil || record.UserID != userID || record.ParentID == nil ||
			*record.ParentID == 0 || *record.ParentID == record.ID || record.Status != "ACTIVE" || record.EntryType != "FILE" ||
			record.Revision == 0 || !modifiedPosition.IsValid() || record.CreatedAt.IsZero() || record.UpdatedAt.IsZero() ||
			record.Name == "" || !utf8.ValidString(record.Name) {
			return nil, domain.ErrInvalidFileEntryData
		}
		position := domain.RecentEntryPosition{LastOpenedAt: *record.LastOpenedAt, ID: record.ID}
		if !position.IsValid() || (upper != nil && (position.LastOpenedAt.After(upper.LastOpenedAt) ||
			(position.LastOpenedAt.Equal(upper.LastOpenedAt) && position.ID >= upper.ID))) {
			return nil, domain.ErrInvalidFileEntryData
		}
		parentCopy := *record.ParentID
		entries = append(entries, domain.RecentEntryCandidate{
			Entry: domain.FileEntry{
				ID:         record.ID,
				UserID:     record.UserID,
				ParentID:   &parentCopy,
				Name:       record.Name,
				EntryType:  record.EntryType,
				FileSize:   record.FileSize,
				Revision:   record.Revision,
				Status:     record.Status,
				CreatedAt:  record.CreatedAt,
				ModifiedAt: record.ModifiedAt,
				UpdatedAt:  record.UpdatedAt,
			},
			LastOpenedAt: position.LastOpenedAt,
		})
		upper = &position
	}

	// 5. 根据额外候选生成 HasMore，返回候选页。
	hasMore := len(entries) > query.PageSize
	if hasMore {
		entries = entries[:query.PageSize]
	}
	return &domain.RecentEntryPage{Entries: entries, HasMore: hasMore}, nil
}
