package repository

import (
	"database/sql"
	"fmt"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Store 封装业务数据访问，共用由启动入口管理的连接池。
type Store struct {
	// database 只在 Repository 内部使用，不向 HTTP 或 Service 暴露 ORM 操作。
	database *gorm.DB
	// loginUserID 仅在登录事务锁定并复查用户后设置，限定设备写入归属。
	loginUserID uint64
}

// NewStore 用现有连接池装配 GORM，不创建连接池、不执行探测或迁移，也不接管关闭操作。
func NewStore(connectionPool *sql.DB) (*Store, error) {
	// 1. 检查连接池参数，避免静默创建其他连接。
	if connectionPool == nil {
		return nil, fmt.Errorf("创建数据访问入口需要现有 MySQL 连接池")
	}

	// 2. 复用已有池，禁用隐式网络探测和原始 SQL 日志。
	database, err := gorm.Open(mysql.New(mysql.Config{
		Conn:                      connectionPool,
		SkipInitializeWithVersion: true,
	}), &gorm.Config{
		DisableAutomaticPing: true,
		Logger:               logger.Discard,
	})
	if err != nil {
		return nil, fmt.Errorf("装配 MySQL 数据访问入口失败: %w", err)
	}

	// 3. 返回数据访问入口，池仍由原调用方关闭。
	return &Store{database: database}, nil
}
