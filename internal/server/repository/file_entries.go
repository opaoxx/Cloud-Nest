package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
)

// FindUserFileEntries 按已认证用户批量读取目录项基础记录，供同一快照内的祖先检查使用。
// 输入至多 MaxGlobalEntryCandidates 个非零 ID，重复值合并，空批次返回初始化后的空映射。
// 缺失或跨账号 ID 在结果中缺省，根目录及生命周期原值由服务继续检查。
func (store *Store) FindUserFileEntries(ctx context.Context, userID uint64, entryIDs []uint64) (map[uint64]domain.FileEntry, error) {
	// 1. 检查上下文、用户和批次大小，设置查询期限。
	if ctx == nil || userID == 0 || len(entryIDs) > domain.MaxGlobalEntryCandidates {
		return nil, fmt.Errorf("批量读取目录项需要上下文、非零用户及至多 %d 个目录项 ID", domain.MaxGlobalEntryCandidates)
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("批量读取目录项失败: %w", err)
	}

	// 2. 校验并复制去重后的 ID，处理空批次。
	requestedIDs := make(map[uint64]bool, len(entryIDs))
	uniqueIDs := make([]uint64, 0, len(entryIDs))
	for _, entryID := range entryIDs {
		if entryID == 0 {
			return nil, fmt.Errorf("批量读取目录项需要非零目录项 ID")
		}
		if !requestedIDs[entryID] {
			requestedIDs[entryID] = true
			uniqueIDs = append(uniqueIDs, entryID)
		}
	}
	if len(uniqueIDs) == 0 {
		return make(map[uint64]domain.FileEntry), nil
	}

	// 3. 按用户和 ID 集合读取基础字段。
	var records []models.FileEntry
	queryError := store.database.WithContext(queryContext).
		Select("id", "user_id", "parent_id", "name", "entry_type", "file_size", "revision", "status", "created_at", "modified_at", "updated_at").
		Where("user_id = ? AND id IN ?", userID, uniqueIDs).
		Find(&records).Error

	// 4. 处理取消、扫描及数据库故障。
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("批量读取目录项已取消: %w", err)
	}
	if queryError != nil {
		return nil, fmt.Errorf("批量读取目录项失败: %w", queryError)
	}

	// 5. 核对归属和标识，复制记录及父目录值。
	entries := make(map[uint64]domain.FileEntry, len(records))
	for _, record := range records {
		if !requestedIDs[record.ID] || record.UserID != userID || record.Revision == 0 || (record.ParentID != nil && *record.ParentID == 0) {
			return nil, domain.ErrInvalidFileEntryData
		}
		// 每个请求 ID 仅接受一条记录，重复行会触发数据错误。
		requestedIDs[record.ID] = false
		var parentID *uint64
		if record.ParentID != nil {
			parentCopy := *record.ParentID
			parentID = &parentCopy
		}
		entries[record.ID] = domain.FileEntry{
			ID:         record.ID,
			UserID:     record.UserID,
			ParentID:   parentID,
			Name:       record.Name,
			EntryType:  record.EntryType,
			FileSize:   record.FileSize,
			Revision:   record.Revision,
			Status:     record.Status,
			CreatedAt:  record.CreatedAt,
			ModifiedAt: record.ModifiedAt,
			UpdatedAt:  record.UpdatedAt,
		}
	}
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("交付批量目录项前已取消: %w", err)
	}
	return entries, nil
}
