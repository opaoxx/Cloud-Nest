package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
	"gorm.io/gorm"
)

// FindUserFileEntry 按已验证用户归属读取指定文件或目录的基础记录。
// userID 取自已认证调用方，生命周期及祖先可见性由后续服务检查。
// 缺失或跨账号目标返回 ErrFileEntryNotFound，数据异常与数据库故障各自返回错误。
func (store *Store) FindUserFileEntry(ctx context.Context, userID, entryID uint64) (*domain.FileEntry, error) {
	// 1. 检查用户、目录项 ID 和上下文，设置查询期限。
	if ctx == nil || userID == 0 || entryID == 0 {
		return nil, fmt.Errorf("查询目录项需要上下文、非零用户 ID 和目录项 ID")
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("查询目录项失败: %w", err)
	}

	// 2. 按用户归属读取指定目录项的基础字段。
	var record models.FileEntry
	queryError := store.database.WithContext(queryContext).
		Select("id", "user_id", "parent_id", "name", "entry_type", "file_size", "revision", "status", "created_at", "modified_at", "updated_at").
		Where("user_id = ? AND id = ?", userID, entryID).
		Take(&record).Error

	// 3. 处理取消、未找到及数据库故障。
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("查询目录项已取消: %w", err)
	}
	if errors.Is(queryError, gorm.ErrRecordNotFound) {
		return nil, domain.ErrFileEntryNotFound
	}
	if queryError != nil {
		return nil, fmt.Errorf("查询目录项失败: %w", queryError)
	}

	// 4. 核对归属与标识，映射目录项基础记录。
	if record.ID != entryID || record.UserID != userID || record.Revision == 0 || (record.ParentID != nil && *record.ParentID == 0) {
		return nil, domain.ErrInvalidFileEntryData
	}
	return &domain.FileEntry{
		ID:         record.ID,
		UserID:     record.UserID,
		ParentID:   record.ParentID,
		Name:       record.Name,
		EntryType:  record.EntryType,
		FileSize:   record.FileSize,
		Revision:   record.Revision,
		Status:     record.Status,
		CreatedAt:  record.CreatedAt,
		ModifiedAt: record.ModifiedAt,
		UpdatedAt:  record.UpdatedAt,
	}, nil
}
