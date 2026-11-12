package repository

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
)

// RotateRefreshToken 在刷新用户事务中轮换当前摘要和代次。
// ctx 与 Store 来自 WithRefreshUserTransaction 的回调，新摘要来自新生成的刷新原文。
// 返回结果须等待外层事务提交成功后确认生效，失败时回调应将错误返回外层。
func (store *Store) RotateRefreshToken(ctx context.Context, newHash [sha256.Size]byte) (*domain.VerifiedRefreshSession, error) {
	// 1. 确认刷新事务、上下文、新摘要和代次有效。
	if ctx == nil || store.refreshSession == nil || store.loginUserID == 0 {
		return nil, fmt.Errorf("轮换刷新摘要需要上下文和已复查会话的刷新事务")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return nil, fmt.Errorf("轮换刷新摘要必须使用刷新事务 Store")
	}
	current := *store.refreshSession
	if current.Session.UserID != store.loginUserID || current.Session.ID == 0 || current.Session.DeviceID == 0 ||
		current.Session.ExpiresAt.IsZero() || current.TokenGeneration == 0 || current.TokenGeneration == ^uint64(0) {
		return nil, domain.ErrInvalidSessionData
	}
	if newHash == current.TokenHash {
		return nil, fmt.Errorf("轮换刷新摘要需要与当前值不同的新摘要")
	}
	updateContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := updateContext.Err(); err != nil {
		return nil, err
	}
	checkedAt := time.Now().UTC()
	if !current.Session.ExpiresAt.After(checkedAt) {
		return nil, domain.ErrSessionExpired
	}

	// 2. 使用旧摘要和旧代次作为条件，更新新摘要及下一代次。
	nextGeneration := current.TokenGeneration + 1
	result := store.database.WithContext(updateContext).Exec(
		"UPDATE auth_sessions SET refresh_token_hash = ?, token_generation = ?, updated_at = UTC_TIMESTAMP(6) "+
			"WHERE user_id = ? AND device_id = ? AND id = ? AND refresh_token_hash = ? AND token_generation = ? "+
			"AND revoked_at IS NULL AND expires_at > ?",
		newHash[:], nextGeneration, current.Session.UserID, current.Session.DeviceID, current.Session.ID,
		current.TokenHash[:], current.TokenGeneration, checkedAt)

	// 3. 处理取消、摘要冲突及更新行数异常。
	if err := updateContext.Err(); err != nil {
		return nil, err
	}
	if result.Error != nil {
		var databaseError *mysql.MySQLError
		if errors.As(result.Error, &databaseError) && databaseError.Number == 1062 {
			return nil, domain.ErrRefreshTokenConflict
		}
		return nil, fmt.Errorf("轮换刷新摘要失败: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return nil, domain.ErrInvalidRefreshToken
	}
	if result.RowsAffected != 1 {
		return nil, domain.ErrInvalidSessionData
	}

	// 4. 返回轮换结果，等待外层事务提交确认。
	rotated := current
	rotated.TokenHash = newHash
	rotated.TokenGeneration = nextGeneration
	return &rotated, nil
}
