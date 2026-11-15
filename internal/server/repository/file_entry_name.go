package repository

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
	"gorm.io/gorm"
)

// FindUserActiveFileEntryByName 按用户、真实父目录和已生成名称键读取活动同名项。
// userID 来自已认证身份，nameKey 来自统一名称规则，父目录可见性由调用服务检查。
// 未找到返回 ErrFileEntryNotFound，写入时由业务事务与唯一键处理并发名称冲突。
func (store *Store) FindUserActiveFileEntryByName(ctx context.Context, userID, parentID uint64, nameKey []byte) (*domain.FileEntry, error) {
	// 1. 检查用户、父目录、名称键和上下文，设置查询期限。
	if ctx == nil || userID == 0 || parentID == 0 || len(nameKey) == 0 || len(nameKey) > 768 {
		return nil, fmt.Errorf("查询活动同名目录项需要上下文、非零用户和父目录 ID 及 1～768 字节的名称键")
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("查询活动同名目录项失败: %w", err)
	}
	key := bytes.Clone(nameKey)

	// 2. 按用户、父目录和活动名称键读取基础字段。
	var record models.FileEntry
	queryError := store.database.WithContext(queryContext).
		Select("id", "user_id", "parent_id", "name", "entry_type", "file_size", "revision", "status", "created_at", "modified_at", "updated_at").
		Where("user_id = ? AND parent_id = ? AND active_name_key = ? AND status = ?", userID, parentID, key, "ACTIVE").
		Take(&record).Error

	// 3. 处理取消、未找到及数据库故障。
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("查询活动同名目录项已取消: %w", err)
	}
	if errors.Is(queryError, gorm.ErrRecordNotFound) {
		return nil, domain.ErrFileEntryNotFound
	}
	if queryError != nil {
		return nil, fmt.Errorf("查询活动同名目录项失败: %w", queryError)
	}

	// 4. 核对结果范围，返回独立的目录项记录。
	if record.ID == 0 || record.UserID != userID || record.ParentID == nil || *record.ParentID != parentID || record.Status != "ACTIVE" || record.Revision == 0 {
		return nil, domain.ErrInvalidFileEntryData
	}
	parentCopy := *record.ParentID
	return &domain.FileEntry{
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
	}, nil
}
