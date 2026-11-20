package repository

import (
	"context"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
)

// ListUserTrashEntryCandidates 分批读取当前用户的回收站候选。
// before 为 nil 时从开头读取，limit 为 1～100。调用方在同一快照内核对完整祖先链。
func (store *Store) ListUserTrashEntryCandidates(ctx context.Context, userID uint64, limit int, before *domain.TrashEntryPosition) (*domain.TrashEntryPage, error) {
	// 1. 校验用户、批大小和续页位置，为查询设置期限。
	if ctx == nil || userID == 0 || limit < 1 || limit > 100 {
		return nil, fmt.Errorf("读取回收站候选需要上下文、非零用户 ID 和 1～100 的批大小")
	}
	var upper *domain.TrashEntryPosition
	if before != nil {
		position := *before
		if !position.IsValid() {
			return nil, fmt.Errorf("回收站候选翻页位置需要非零 ID 和数据库微秒时间")
		}
		position.DeletedAt = position.DeletedAt.UTC()
		upper = &position
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("读取回收站候选已取消: %w", err)
	}

	// 2. 读取当前用户的 TRASHED 项，按双降序多查一条。
	statement := store.database.WithContext(queryContext).
		Select("id", "user_id", "parent_id", "name", "entry_type", "file_size", "revision", "status", "created_at", "modified_at", "updated_at", "deleted_at").
		Where("user_id = ? AND status = ?", userID, "TRASHED")
	if upper != nil {
		statement = statement.Where("(deleted_at < ? OR (deleted_at = ? AND id < ?))", upper.DeletedAt, upper.DeletedAt, upper.ID)
	}
	var records []models.FileEntryTrash
	queryError := statement.Order("deleted_at DESC, id DESC").Limit(limit + 1).Find(&records).Error

	// 3. 处理查询失败和取消，核对记录及顺序。
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("读取回收站候选已取消: %w", err)
	}
	if queryError != nil {
		return nil, fmt.Errorf("读取回收站候选失败: %w", queryError)
	}
	if len(records) > limit+1 {
		return nil, domain.ErrInvalidFileEntryData
	}
	entries := make([]domain.TrashedFileEntry, 0, len(records))
	for _, record := range records {
		if record.UserID != userID || record.Status != "TRASHED" || record.DeletedAt == nil ||
			record.ParentID == nil || *record.ParentID == 0 || *record.ParentID == record.ID ||
			record.Revision == 0 || record.Name == "" || !utf8.ValidString(record.Name) ||
			(record.EntryType != "FILE" && record.EntryType != "DIR") || (record.EntryType == "DIR" && record.FileSize != 0) ||
			record.CreatedAt.IsZero() || record.ModifiedAt.IsZero() || record.UpdatedAt.IsZero() {
			return nil, domain.ErrInvalidFileEntryData
		}
		position := domain.TrashEntryPosition{DeletedAt: record.DeletedAt.UTC(), ID: record.ID}
		if !position.IsValid() || (upper != nil && (position.DeletedAt.After(upper.DeletedAt) ||
			(position.DeletedAt.Equal(upper.DeletedAt) && position.ID >= upper.ID))) {
			return nil, domain.ErrInvalidFileEntryData
		}
		parentID := *record.ParentID
		entries = append(entries, domain.TrashedFileEntry{
			FileEntry: domain.FileEntry{
				ID: record.ID, UserID: record.UserID, ParentID: &parentID, Name: record.Name, EntryType: record.EntryType,
				FileSize: record.FileSize, Revision: record.Revision, Status: record.Status,
				CreatedAt: record.CreatedAt, ModifiedAt: record.ModifiedAt, UpdatedAt: record.UpdatedAt,
			},
			DeletedAt: position.DeletedAt,
		})
		upper = &position
	}

	// 4. 用额外的一条判断 HasMore，返回请求大小的候选批次。
	hasMore := len(entries) > limit
	if hasMore {
		entries = entries[:limit:limit]
	}
	return &domain.TrashEntryPage{Entries: entries, HasMore: hasMore}, nil
}
