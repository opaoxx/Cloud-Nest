package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
)

// ListUserDirectoryEntries 按用户和真实父目录读取 ACTIVE 子项。
// before 为 nil 时读取首页，后续页使用上一页末项的修改时间与 ID，页大小为 1～100。
// 调用方在同一快照中组合父目录可见性检查，userID 来自已认证身份。
func (store *Store) ListUserDirectoryEntries(ctx context.Context, userID, parentID uint64, before *domain.FileEntryPosition, pageSize int) (*domain.FileEntryPage, error) {
	// 1. 检查用户、父目录、分页位置和上下文，设置查询期限。
	if ctx == nil || userID == 0 || parentID == 0 || pageSize < 1 || pageSize > 100 {
		return nil, fmt.Errorf("读取目录子项需要上下文、非零用户和父目录 ID 及 1～100 的页大小")
	}
	var upper *domain.FileEntryPosition
	if before != nil {
		position := *before
		if !position.IsValid() {
			return nil, fmt.Errorf("目录翻页位置需要非零 ID 和数据库微秒时间")
		}
		position.ModifiedAt = position.ModifiedAt.UTC()
		upper = &position
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("读取目录子项失败: %w", err)
	}

	// 2. 按用户、父目录及活动状态读取一页和额外一条记录。
	query := store.database.WithContext(queryContext).
		Select("id", "user_id", "parent_id", "name", "entry_type", "file_size", "revision", "status", "created_at", "modified_at", "updated_at").
		Where("user_id = ? AND parent_id = ? AND status = ?", userID, parentID, "ACTIVE")
	if upper != nil {
		query = query.Where("(modified_at < ? OR (modified_at = ? AND id < ?))", upper.ModifiedAt, upper.ModifiedAt, upper.ID)
	}
	var records []models.FileEntry
	queryError := query.Order("modified_at DESC, id DESC").Limit(pageSize + 1).Find(&records).Error

	// 3. 处理取消、扫描及数据库故障。
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("读取目录子项已取消: %w", err)
	}
	if queryError != nil {
		return nil, fmt.Errorf("读取目录子项失败: %w", queryError)
	}

	// 4. 核对归属、父目录和排序，映射目录项记录。
	entries := make([]domain.FileEntry, 0, len(records))
	for _, record := range records {
		position := domain.FileEntryPosition{ModifiedAt: record.ModifiedAt, ID: record.ID}
		if record.UserID != userID || record.ParentID == nil || *record.ParentID != parentID || record.Status != "ACTIVE" || record.Revision == 0 || !position.IsValid() {
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

	// 5. 根据额外记录判断后续页，返回本页结果。
	hasMore := len(entries) > pageSize
	if hasMore {
		entries = entries[:pageSize]
	}
	return &domain.FileEntryPage{Entries: entries, HasMore: hasMore}, nil
}
