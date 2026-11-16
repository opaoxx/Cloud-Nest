package repository

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
)

// ListUserNameEntryCandidates 按认证用户读取名称升序的全局活动普通目录项候选。
// 当前支持 all/starred 的 name_asc、名称关键词及合法 kind，after 为 nil 时从首页开始。
// 搜索词按原文做包含匹配，模式字符使用显式转义，匹配沿用 name 的排序规则。
// 调用方在同一快照内检查候选及完整祖先链，再决定对外交付的记录和翻页位置。
func (store *Store) ListUserNameEntryCandidates(ctx context.Context, userID uint64, query domain.GlobalEntryQuery, after *domain.NameEntryPosition) (*domain.NameEntryPage, error) {
	// 1. 校验用户、查询和分页位置，复制名称键并设置查询期限。
	if ctx == nil || userID == 0 || !query.IsValid() ||
		(query.Scope != domain.GlobalEntryScopeAll && query.Scope != domain.GlobalEntryScopeStarred) ||
		query.Sort != domain.GlobalEntrySortNameAsc {
		return nil, fmt.Errorf("读取全局候选需要上下文、非零用户及 all/starred、name_asc 的合法查询")
	}

	var lower *domain.NameEntryPosition
	if after != nil {
		position := *after
		if !position.IsValid() {
			return nil, fmt.Errorf("名称候选翻页位置需要非零 ID 和 1～768 字节名称键")
		}
		position.NameKey = bytes.Clone(position.NameKey)
		lower = &position
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("读取全局候选失败: %w", err)
	}

	// 2. 组合范围、搜索、分类及名称键/ID 升序续页条件。
	statement := store.database.WithContext(queryContext).Table("file_entries").
		Select("id", "user_id", "parent_id", "name", "entry_type", "file_size", "revision", "status", "created_at", "modified_at", "updated_at", "name_key").
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
	if lower != nil {
		statement = statement.Where("(name_key > ? OR (name_key = ? AND id > ?))", lower.NameKey, lower.NameKey, lower.ID)
	}
	var records []struct {
		models.FileEntry
		NameKey []byte `gorm:"column:name_key;->"`
	}
	queryError := statement.Order("name_key ASC, id ASC").Limit(query.PageSize + 1).Find(&records).Error

	// 3. 处理取消、扫描及数据库错误。
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("读取全局候选已取消: %w", err)
	}
	if queryError != nil {
		return nil, fmt.Errorf("读取全局候选失败: %w", queryError)
	}

	// 4. 核对候选结构和严格升序，复制返回数据。
	if len(records) > query.PageSize+1 {
		return nil, domain.ErrInvalidFileEntryData
	}
	entries := make([]domain.NameEntryCandidate, 0, len(records))
	for _, record := range records {
		position := domain.NameEntryPosition{NameKey: record.NameKey, ID: record.ID}
		modifiedPosition := domain.FileEntryPosition{ModifiedAt: record.ModifiedAt, ID: record.ID}
		// 4.1. 核对返回记录与请求的文件/目录类型一致。
		if entryType != "" && record.EntryType != entryType {
			return nil, domain.ErrInvalidFileEntryData
		}
		if record.UserID != userID || record.ParentID == nil || *record.ParentID == 0 || *record.ParentID == record.ID || record.Status != "ACTIVE" || record.Revision == 0 || !position.IsValid() || !modifiedPosition.IsValid() {
			return nil, domain.ErrInvalidFileEntryData
		}
		if record.Name == "" || !utf8.ValidString(record.Name) || record.CreatedAt.IsZero() || record.UpdatedAt.IsZero() ||
			(record.EntryType != "FILE" && record.EntryType != "DIR") || (record.EntryType == "DIR" && record.FileSize != 0) {
			return nil, domain.ErrInvalidFileEntryData
		}
		if lower != nil {
			comparison := bytes.Compare(position.NameKey, lower.NameKey)
			if comparison < 0 || (comparison == 0 && position.ID <= lower.ID) {
				return nil, domain.ErrInvalidFileEntryData
			}
		}
		parentCopy := *record.ParentID
		entries = append(entries, domain.NameEntryCandidate{
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
			NameKey: bytes.Clone(position.NameKey),
		})
		lower = &position
	}

	// 5. 用额外一条候选判断 HasMore。
	hasMore := len(entries) > query.PageSize
	if hasMore {
		entries = entries[:query.PageSize]
	}
	return &domain.NameEntryPage{Entries: entries, HasMore: hasMore}, nil
}
