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

// ListUserSizeEntryCandidates 按认证用户读取大小降序的全局活动普通目录项候选。
// 当前支持 all/starred 的 size_desc、名称关键词及合法 kind，before 为 nil 时从首页开始。
// 搜索词按原文做包含匹配，模式字符使用显式转义，匹配沿用 name 的排序规则。
// 调用方在同一快照内检查候选及完整祖先链，再决定对外交付的记录和翻页位置。
func (store *Store) ListUserSizeEntryCandidates(ctx context.Context, userID uint64, query domain.GlobalEntryQuery, before *domain.SizeEntryPosition) (*domain.FileEntryPage, error) {
	// 1. 校验用户、查询和分页位置，设置查询期限。
	if ctx == nil || userID == 0 || !query.IsValid() ||
		(query.Scope != domain.GlobalEntryScopeAll && query.Scope != domain.GlobalEntryScopeStarred) ||
		query.Sort != domain.GlobalEntrySortSizeDesc {
		return nil, fmt.Errorf("读取全局候选需要上下文、非零用户及 all/starred、size_desc 的合法查询")
	}

	var upper *domain.SizeEntryPosition
	if before != nil {
		position := *before
		if !position.IsValid() {
			return nil, fmt.Errorf("大小候选翻页位置需要非零 ID")
		}
		upper = &position
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("读取全局候选失败: %w", err)
	}

	// 2. 组合范围、搜索、分类及大小/ID 降序续页条件。
	statement := store.database.WithContext(queryContext).
		Select("id", "user_id", "parent_id", "name", "entry_type", "file_size", "revision", "status", "created_at", "modified_at", "updated_at").
		Where("user_id = ? AND status = ? AND parent_id IS NOT NULL", userID, "ACTIVE")
	// 2.1. 在 starred 范围内限定当前用户已有收藏时间的目录项。
	if query.Scope == domain.GlobalEntryScopeStarred {
		statement = statement.Where("EXISTS (SELECT 1 FROM user_entry_preferences WHERE user_entry_preferences.user_id = ? AND user_entry_preferences.entry_id = file_entries.id AND user_entry_preferences.starred_at IS NOT NULL)", userID)
	}

	// 2.2. 按合法 kind 绑定目录类型或文件分类。
	entryType := ""
	if query.Kind == "folder" {
		entryType = "DIR"
		statement = statement.Where("entry_type = ?", entryType)
	} else if query.Kind != "" {
		entryType = "FILE"
		statement = statement.Where("entry_type = ? AND category = ?", entryType, strings.ToUpper(query.Kind))
	}
	// 2.3. 转义搜索词中的模式字符，绑定名称包含匹配条件。
	if query.Search != "" {
		escapedSearch := strings.NewReplacer("=", "==", "%", "=%", "_", "=_").Replace(query.Search)
		statement = statement.Where("name LIKE ? ESCAPE '='", "%"+escapedSearch+"%")
	}
	if upper != nil {
		statement = statement.Where("(file_size < ? OR (file_size = ? AND id < ?))", upper.FileSize, upper.FileSize, upper.ID)
	}
	var records []models.FileEntry
	queryError := statement.Order("file_size DESC, id DESC").Limit(query.PageSize + 1).Find(&records).Error

	// 3. 处理取消、扫描及数据库错误。
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
	seenIDs := make(map[uint64]bool, len(records))
	for _, record := range records {
		position := domain.SizeEntryPosition{FileSize: record.FileSize, ID: record.ID}
		modifiedPosition := domain.FileEntryPosition{ModifiedAt: record.ModifiedAt, ID: record.ID}
		// 4.1. 核对返回记录与请求的文件/目录类型一致。
		if entryType != "" && record.EntryType != entryType {
			return nil, domain.ErrInvalidFileEntryData
		}
		if seenIDs[record.ID] || record.UserID != userID || record.ParentID == nil || *record.ParentID == 0 || *record.ParentID == record.ID || record.Status != "ACTIVE" || record.Revision == 0 || !position.IsValid() || !modifiedPosition.IsValid() {
			return nil, domain.ErrInvalidFileEntryData
		}
		if record.Name == "" || !utf8.ValidString(record.Name) || record.CreatedAt.IsZero() || record.UpdatedAt.IsZero() ||
			(record.EntryType != "FILE" && record.EntryType != "DIR") || (record.EntryType == "DIR" && record.FileSize != 0) {
			return nil, domain.ErrInvalidFileEntryData
		}
		if upper != nil && (position.FileSize > upper.FileSize || (position.FileSize == upper.FileSize && position.ID >= upper.ID)) {
			return nil, domain.ErrInvalidFileEntryData
		}
		seenIDs[record.ID] = true
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

	// 5. 根据额外候选判断后续页。
	hasMore := len(entries) > query.PageSize
	if hasMore {
		entries = entries[:query.PageSize]
	}
	return &domain.FileEntryPage{Entries: entries, HasMore: hasMore}, nil
}
