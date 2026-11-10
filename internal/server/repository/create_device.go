package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
	"gorm.io/gorm"
)

// CreateLoginDevice 在 WithLoginUserTransaction 回调中创建新设备，归属取自已锁定的用户。
// 三个输入字段应先通过登录 DTO 校验；使用回调上下文，不能用普通 Store 直接写入。
// 返回对象只表示事务内插入成功，必须等外层事务返回 nil 才能确认创建完成。
func (store *Store) CreateLoginDevice(ctx context.Context, installationID, name, platform string) (*domain.LoginDevice, error) {
	// 1. 确认当前 Store 已在用户事务内，并检查上下文。
	if ctx == nil || store.loginUserID == 0 {
		return nil, fmt.Errorf("创建设备需要上下文和已锁定用户的登录事务")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return nil, fmt.Errorf("创建设备必须使用登录事务 Store")
	}
	writeContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := writeContext.Err(); err != nil {
		return nil, fmt.Errorf("创建登录设备失败: %w", err)
	}

	// 2. 使用锁定的用户 ID，组装本次允许写入的字段。
	record := models.NewLoginDevice{
		UserID:         store.loginUserID,
		InstallationID: installationID,
		Name:           name,
		Platform:       platform,
	}

	// 3. 插入设备，区分唯一键冲突与数据库故障。
	result := store.database.WithContext(writeContext).
		Select("user_id", "installation_id", "name", "platform").Create(&record)
	if result.Error != nil {
		var databaseError *mysql.MySQLError
		if errors.As(result.Error, &databaseError) && databaseError.Number == 1062 {
			return nil, domain.ErrDeviceAlreadyExists
		}
		return nil, fmt.Errorf("创建登录设备失败: %w", result.Error)
	}
	if err := writeContext.Err(); err != nil {
		return nil, fmt.Errorf("创建登录设备已取消: %w", err)
	}
	if result.RowsAffected != 1 || record.ID == 0 {
		return nil, fmt.Errorf("创建登录设备未取得单条写入结果和设备 ID")
	}

	// 4. 返回新设备；整次事务提交成功后，创建才确认完成。
	return &domain.LoginDevice{
		ID:             record.ID,
		UserID:         record.UserID,
		InstallationID: record.InstallationID,
		Name:           record.Name,
		Platform:       record.Platform,
	}, nil
}
