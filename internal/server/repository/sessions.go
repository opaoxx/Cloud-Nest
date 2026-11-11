package repository

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// CreateLoginSession 在登录用户事务中为有效设备创建会话，摘要来自已生成的刷新令牌。
// 到期时间由调用方提供，返回对象在外层事务提交成功后确认生效。
func (store *Store) CreateLoginSession(ctx context.Context, deviceID uint64, tokenHash [sha256.Size]byte, expiresAt time.Time) (*domain.LoginSession, error) {
	// 1. 检查用户事务、设备 ID、到期时间和上下文。
	if ctx == nil || store.loginUserID == 0 || deviceID == 0 {
		return nil, fmt.Errorf("创建会话需要上下文、登录用户事务和非零设备 ID")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return nil, fmt.Errorf("创建会话必须使用登录事务 Store")
	}
	expiresAt = expiresAt.UTC().Truncate(time.Microsecond)
	if expiresAt.Year() < 1000 || expiresAt.Year() > 9999 || !expiresAt.After(time.Now()) {
		return nil, fmt.Errorf("会话到期时间须在未来并处于数据库支持范围")
	}
	sessionContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := sessionContext.Err(); err != nil {
		return nil, err
	}

	// 2. 锁定当前用户的设备，确认设备可以建立会话。
	var device models.LoginDevice
	queryError := store.database.WithContext(sessionContext).
		Select("id", "user_id", "revoked_at").
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("user_id = ? AND id = ?", store.loginUserID, deviceID).Take(&device).Error
	if err := sessionContext.Err(); err != nil {
		return nil, err
	}
	if errors.Is(queryError, gorm.ErrRecordNotFound) {
		return nil, domain.ErrDeviceNotFound
	}
	if queryError != nil {
		return nil, fmt.Errorf("锁定会话设备失败: %w", queryError)
	}
	if device.ID != deviceID || device.UserID != store.loginUserID {
		return nil, domain.ErrInvalidDeviceData
	}
	if device.RevokedAt != nil {
		return nil, domain.ErrDeviceRevoked
	}
	if !expiresAt.After(time.Now()) {
		return nil, fmt.Errorf("创建会话前到期时间已过")
	}

	// 3. 插入用户、设备、令牌摘要和到期时间，处理写入错误。
	record := models.NewLoginSession{
		UserID:           store.loginUserID,
		DeviceID:         deviceID,
		RefreshTokenHash: tokenHash[:],
		ExpiresAt:        expiresAt,
	}
	result := store.database.WithContext(sessionContext).
		Select("user_id", "device_id", "refresh_token_hash", "expires_at").Create(&record)
	if result.Error != nil {
		var databaseError *mysql.MySQLError
		if errors.As(result.Error, &databaseError) && databaseError.Number == 1062 {
			return nil, domain.ErrRefreshTokenConflict
		}
		return nil, fmt.Errorf("创建登录会话失败: %w", result.Error)
	}
	if err := sessionContext.Err(); err != nil {
		return nil, err
	}
	if result.RowsAffected != 1 || record.ID == 0 {
		return nil, domain.ErrInvalidSessionData
	}

	// 4. 返回会话结果，等待外层事务提交确认。
	return &domain.LoginSession{
		ID:        record.ID,
		UserID:    record.UserID,
		DeviceID:  record.DeviceID,
		ExpiresAt: record.ExpiresAt,
	}, nil
}
