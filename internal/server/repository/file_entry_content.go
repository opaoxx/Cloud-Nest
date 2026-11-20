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

// FindUserFileEntryContent 按已验证用户归属读取目录项的内容引用。
// 调用方在同一快照中检查目标及祖先可见性；缺失或跨账号返回 ErrFileEntryNotFound。
func (store *Store) FindUserFileEntryContent(ctx context.Context, userID, entryID uint64) (*domain.FileEntryContent, error) {
	// 1. 检查查询参数并设置期限。
	if ctx == nil || userID == 0 || entryID == 0 {
		return nil, fmt.Errorf("查询目录项内容引用需要上下文、非零用户 ID 和目录项 ID")
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("查询目录项内容引用失败: %w", err)
	}

	// 2. 按用户和目录项 ID 读取内容引用。
	var record models.FileEntryContent
	queryError := store.database.WithContext(queryContext).
		Select("id", "user_id", "object_id", "mime_type").
		Where("user_id = ? AND id = ?", userID, entryID).
		Take(&record).Error

	// 3. 区分取消、缺失和数据库故障。
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("查询目录项内容引用已取消: %w", err)
	}
	if errors.Is(queryError, gorm.ErrRecordNotFound) {
		return nil, domain.ErrFileEntryNotFound
	}
	if queryError != nil {
		return nil, fmt.Errorf("查询目录项内容引用失败: %w", queryError)
	}

	// 4. 核对返回记录并交付内容引用，保留合法的空对象及 MIME。
	if record.ID != entryID || record.UserID != userID || (record.ObjectID != nil && *record.ObjectID == 0) {
		return nil, domain.ErrInvalidFileEntryData
	}
	return &domain.FileEntryContent{
		ID:       record.ID,
		UserID:   record.UserID,
		ObjectID: record.ObjectID,
		MIMEType: record.MIMEType,
	}, nil
}
