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

// FindUserProfile 读取已验证用户的资料、活动根目录和配额快照。
// userID 须来自凭证验证或请求授权，账号状态交给服务层判断。
// 未找到返回 ErrAccountNotFound，缺少根目录或配额异常返回 ErrInvalidAccountData。
func (store *Store) FindUserProfile(ctx context.Context, userID uint64) (*domain.UserProfile, error) {
	// 1. 检查用户 ID 和上下文，设置查询期限。
	if ctx == nil || userID == 0 {
		return nil, fmt.Errorf("查询用户资料需要上下文和非零用户 ID")
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("查询用户资料失败: %w", err)
	}

	// 2. 关联查询用户、根目录和配额字段。
	var record models.UserProfile
	queryError := store.database.WithContext(queryContext).
		Table("users AS accounts").
		Select([]string{
			"accounts.id AS id", "accounts.username AS username", "accounts.display_name AS display_name",
			"accounts.status AS status", "roots.id AS root_entry_id",
			"accounts.storage_quota AS quota_bytes", "accounts.used_storage AS used_bytes",
			"accounts.reserved_storage AS reserved_bytes",
		}).
		Joins("LEFT JOIN file_entries AS roots ON roots.root_owner_id = accounts.id "+
			"AND roots.user_id = accounts.id AND roots.parent_id IS NULL "+
			"AND roots.entry_type = ? AND roots.status = ?", "DIR", "ACTIVE").
		Where("accounts.id = ?", userID).
		Take(&record).Error

	// 3. 处理取消、未找到及数据库故障。
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("查询用户资料已取消: %w", err)
	}
	if errors.Is(queryError, gorm.ErrRecordNotFound) {
		return nil, domain.ErrAccountNotFound
	}
	if queryError != nil {
		return nil, fmt.Errorf("查询用户资料失败: %w", queryError)
	}

	// 4. 检查归属和配额数据，返回用户资料。
	if record.ID != userID || record.RootEntryID == nil || *record.RootEntryID == 0 {
		return nil, domain.ErrInvalidAccountData
	}
	// 先比较已用量，再用减法检查预占，避免两个 uint64 相加时溢出。
	if record.UsedBytes > record.QuotaBytes || record.ReservedBytes > record.QuotaBytes-record.UsedBytes {
		return nil, domain.ErrInvalidAccountData
	}
	return &domain.UserProfile{
		ID:            record.ID,
		Username:      record.Username,
		DisplayName:   record.DisplayName,
		Status:        record.Status,
		RootEntryID:   *record.RootEntryID,
		QuotaBytes:    record.QuotaBytes,
		UsedBytes:     record.UsedBytes,
		ReservedBytes: record.ReservedBytes,
	}, nil
}
