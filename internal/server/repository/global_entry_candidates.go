package repository

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
)

// ListUserGlobalEntryCandidates 按认证用户读取全局活动普通目录项候选。
// 当前支持 all/modified_desc、名称关键词及合法 kind，before 为 nil 时从首页开始。
// 搜索词按原文做包含匹配，模式字符使用显式转义，匹配沿用 name 的排序规则。
// 调用方在同一快照内检查候选及完整祖先链，再决定对外交付的记录和翻页位置。
func (store *Store) ListUserGlobalEntryCandidates(ctx context.Context, userID uint64, query domain.GlobalEntryQuery, before *domain.FileEntryPosition) (*domain.FileEntryPage, error) {
	// 1. 检查查询条件、用户、翻页位置和上下文，设置查询期限。
	if ctx == nil || userID == 0 || !query.IsValid() || query.Scope != domain.GlobalEntryScopeAll ||
		query.Sort != domain.GlobalEntrySortModifiedDesc {
		return nil, fmt.Errorf("读取全局候选需要上下文、非零用户及 all/modified_desc 的合法查询")
	}

	var upper *domain.FileEntryPosition
	if before != nil {
		position := *before
		if !position.IsValid() {
			return nil, fmt.Errorf("全局候选翻页位置需要非零 ID 和数据库微秒时间")
		}
		position.ModifiedAt = position.ModifiedAt.UTC()
		upper = &position
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("读取全局候选失败: %w", err)
	}

	// 2. 读取当前用户的活动普通目录项及额外一条候选。
	statement := store.database.WithContext(queryContext).
		Select("id", "user_id", "parent_id", "name", "entry_type", "file_size", "revision", "status", "created_at", "modified_at", "updated_at").
		Where("user_id = ? AND status = ? AND parent_id IS NOT NULL", userID, "ACTIVE")
	// 2.1. 按合法 kind 绑定目录类型或文件分类。
	entryType := ""
	if query.Kind == "folder" {
		entryType = "DIR"
		statement = statement.Where("entry_type = ?", entryType)
	} else if query.Kind != "" {
		entryType = "FILE"
		statement = statement.Where("entry_type = ? AND category = ?", entryType, strings.ToUpper(query.Kind))
	}
	// 2.2. 转义搜索词中的模式字符，绑定名称包含匹配条件。
	if query.Search != "" {
		escapedSearch := strings.NewReplacer("=", "==", "%", "=%", "_", "=_").Replace(query.Search)
		statement = statement.Where("name LIKE ? ESCAPE '='", "%"+escapedSearch+"%")
	}
	if upper != nil {
		statement = statement.Where("(modified_at < ? OR (modified_at = ? AND id < ?))", upper.ModifiedAt, upper.ModifiedAt, upper.ID)
	}
	var records []models.FileEntry
	queryError := statement.Order("modified_at DESC, id DESC").Limit(query.PageSize + 1).Find(&records).Error

	// 3. 处理取消、扫描及数据库故障。
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("读取全局候选已取消: %w", err)
	}
	if queryError != nil {
		return nil, fmt.Errorf("读取全局候选失败: %w", queryError)
	}

	// 4. 核对候选范围、基础结构和排序，复制目录项记录。
	if len(records) > query.PageSize+1 {
		return nil, domain.ErrInvalidFileEntryData
	}
	entries := make([]domain.FileEntry, 0, len(records))
	for _, record := range records {
		position := domain.FileEntryPosition{ModifiedAt: record.ModifiedAt, ID: record.ID}
		// 4.1. 核对返回记录与请求的文件/目录类型一致。
		if entryType != "" && record.EntryType != entryType {
			return nil, domain.ErrInvalidFileEntryData
		}
		if record.UserID != userID || record.ParentID == nil || *record.ParentID == 0 || *record.ParentID == record.ID || record.Status != "ACTIVE" || record.Revision == 0 || !position.IsValid() {
			return nil, domain.ErrInvalidFileEntryData
		}
		if record.Name == "" || !utf8.ValidString(record.Name) || record.CreatedAt.IsZero() || record.UpdatedAt.IsZero() ||
			(record.EntryType != "FILE" && record.EntryType != "DIR") || (record.EntryType == "DIR" && record.FileSize != 0) {
			return nil, domain.ErrInvalidFileEntryData
		}
		if upper != nil && (position.ModifiedAt.After(upper.ModifiedAt) || (position.ModifiedAt.Equal(upper.ModifiedAt) && position.ID >= upper.ID)) {
			return nil, domain.ErrInvalidFileEntryData
		}
		parentCopy := *record.ParentID
		entries = append(entries, domain.FileEntry{
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
		})
		upper = &position
	}

	// 5. 根据额外候选判断后续页，返回候选页。
	hasMore := len(entries) > query.PageSize
	if hasMore {
		entries = entries[:query.PageSize]
	}
	return &domain.FileEntryPage{Entries: entries, HasMore: hasMore}, nil
}
