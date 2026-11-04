// Package repository 管理数据库连接，并承载后续持久化访问逻辑。
package repository

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/config"
)

// OpenMySQL 根据已校验的配置创建连接池，并在最多 5 秒内验证连接可用。
// ctx 可以提供更短期限或取消检查；失败返回 nil 和错误，成功后由调用方关闭连接池。
func OpenMySQL(ctx context.Context, databaseConfig config.DatabaseConfig) (*sql.DB, error) {
	// 1. 根据已校验的配置创建 MySQL 连接器。
	driverConfig := mysql.NewConfig()
	driverConfig.Net = "tcp"
	driverConfig.Addr = databaseConfig.Address
	driverConfig.DBName = databaseConfig.DatabaseName
	driverConfig.User = databaseConfig.Username
	driverConfig.Passwd = databaseConfig.Password
	driverConfig.ParseTime = true
	driverConfig.Loc = time.UTC
	driverConfig.Timeout = 5 * time.Second
	driverConfig.ReadTimeout = 5 * time.Second
	driverConfig.WriteTimeout = 5 * time.Second
	driverConfig.Params = map[string]string{"time_zone": "'+00:00'"}
	
	// 直接创建连接器时，字符集使用专用选项，不能作为普通系统变量放入 Params。
	if err := driverConfig.Apply(mysql.Charset("utf8mb4", "")); err != nil {
		return nil, fmt.Errorf("设置 MySQL 字符集失败: %w", err)
	}
	connector, err := mysql.NewConnector(driverConfig)
	if err != nil {
		return nil, fmt.Errorf("创建 MySQL 连接器失败: %w", err)
	}

	// 2. 创建连接池，设置连接数量和连接寿命。
	connectionPool := sql.OpenDB(connector)
	connectionPool.SetMaxOpenConns(10)
	connectionPool.SetMaxIdleConns(5)
	connectionPool.SetConnMaxLifetime(3 * time.Minute)

	// 3. 在最多 5 秒内检查连接，失败时关闭连接池。
	checkContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := connectionPool.PingContext(checkContext); err != nil {
		_ = connectionPool.Close()
		return nil, fmt.Errorf("检查 MySQL 连通性失败: %w", err)
	}

	// 4. 返回通过检查的连接池，由调用方负责关闭。
	return connectionPool, nil
}
