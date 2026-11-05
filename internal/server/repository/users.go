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

// FindLoginAccountByUsername 查询登录最小信息，用户名参数须先由调用方校验。
// 不存在返回 nil/ErrAccountNotFound，其他失败返回 nil/error，成功包括 ACTIVE 或 DISABLED。
func (store *Store) FindLoginAccountByUsername(ctx context.Context, username string) (*domain.LoginAccount, error) {
	// 1. 设置最多 5 秒的查询期限，保留调用方更短期限与取消通知。
	if ctx == nil {
		return nil, fmt.Errorf("查询登录账号需要上下文")
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("查询登录账号失败: %w", err)
	}

	// 2. 参数绑定用户名，只读取 id、password_hash、status 三列。
	var record models.LoginAccount
	err := store.database.WithContext(queryContext).
		Select("id", "password_hash", "status").
		Where("username = ?", username).
		Take(&record).Error

	// 3. 区分未找到与真正的查询错误，不把数据库故障当作账号不存在。
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrAccountNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("查询登录账号失败: %w", err)
	}

	// 4. 将数据库投影转换为内部业务对象，不输出密码摘要。
	return &domain.LoginAccount{ID: record.ID, PasswordHash: record.PasswordHash, Status: record.Status}, nil
}
