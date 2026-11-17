package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ReserveUserUploadSpace 在 WithUserTransaction 回调中为新上传预占完整声明字节。
// 新任务创建时调用一次，幂等重放由上层恢复已有结果，任务及对象写入共用外层事务。
// 回调须向外返回本方法错误，预占在外层事务提交成功后确认生效。
func (store *Store) ReserveUserUploadSpace(ctx context.Context, sizeBytes uint64) error {
	// 1. 检查已认证用户事务和上下文。
	if ctx == nil || store.authenticatedActor == nil || store.loginUserID == 0 {
		return fmt.Errorf("预占上传空间需要上下文和已认证用户事务")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return fmt.Errorf("预占上传空间必须使用已认证事务 Store")
	}
	actor := *store.authenticatedActor
	if actor.UserID != store.loginUserID || actor.DeviceID == 0 || actor.SessionID == 0 {
		return domain.ErrInvalidSessionData
	}
	updateContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := updateContext.Err(); err != nil {
		return err
	}

	// 2. 当前读锁定用户配额，处理缺失及数据库故障。
	var account models.UserProfile
	queryError := store.database.WithContext(updateContext).Table("users").
		Select("id", "status", "storage_quota AS quota_bytes", "used_storage AS used_bytes", "reserved_storage AS reserved_bytes").
		Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", actor.UserID).Take(&account).Error
	if err := updateContext.Err(); err != nil {
		return err
	}
	if errors.Is(queryError, gorm.ErrRecordNotFound) {
		return domain.ErrAccountNotFound
	}
	if queryError != nil {
		return fmt.Errorf("读取上传配额失败: %w", queryError)
	}

	// 3. 校验配额事实，用减法判断剩余空间。
	if account.ID != actor.UserID {
		return domain.ErrInvalidAccountData
	}
	switch account.Status {
	case "ACTIVE":
	case "DISABLED":
		return domain.ErrAccountDisabled
	default:
		return domain.ErrInvalidAccountData
	}
	if account.UsedBytes > account.QuotaBytes || account.ReservedBytes > account.QuotaBytes-account.UsedBytes {
		return domain.ErrInvalidAccountData
	}
	availableBytes := account.QuotaBytes - account.UsedBytes - account.ReservedBytes
	if sizeBytes > availableBytes {
		return domain.ErrQuotaExceeded
	}

	// 4. 零字节直接成功，否则按原值条件更新预占。
	if sizeBytes == 0 {
		return updateContext.Err()
	}
	reservedBytes := account.ReservedBytes + sizeBytes
	result := store.database.WithContext(updateContext).Exec(
		"UPDATE users SET reserved_storage = ?, updated_at = UTC_TIMESTAMP(6) "+
			"WHERE id = ? AND status = 'ACTIVE' AND storage_quota = ? AND used_storage = ? AND reserved_storage = ?",
		reservedBytes, actor.UserID, account.QuotaBytes, account.UsedBytes, account.ReservedBytes)

	// 5. 检查取消和更新结果，交由外层事务确认。
	if err := updateContext.Err(); err != nil {
		return err
	}
	if result.Error != nil {
		return fmt.Errorf("预占上传空间失败: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return domain.ErrInvalidAccountData
	}
	return nil
}
