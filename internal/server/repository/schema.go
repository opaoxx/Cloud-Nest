package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/migrations"
)

// CheckSchema 在最多 5 秒内只读核对迁移记录是否符合当前程序预期。
// 当前仅接受版本 1、dirty=false 且摘要一致的单条记录，不执行迁移或修复。
// 检查不验证实际表结构，也不代替迁移期间的停机维护流程。
func CheckSchema(ctx context.Context, connectionPool *sql.DB) error {
	// 1. 设置检查期限，计算程序内嵌迁移文件的摘要。
	checkContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	expectedChecksum := fmt.Sprintf("%x", sha256.Sum256([]byte(migrations.InitialSQL)))

	// 2. 只读查询迁移记录，并登记查询结果的关闭操作。
	rows, err := connectionPool.QueryContext(checkContext, "SELECT version, checksum, dirty FROM schema_migrations ORDER BY version LIMIT 2")
	if err != nil {
		var mysqlError *mysql.MySQLError
		if errors.As(err, &mysqlError) && mysqlError.Number == 1146 {
			return fmt.Errorf("缺少 schema_migrations 迁移表，请先使用独立迁移命令初始化: %w", err)
		}
		return fmt.Errorf("读取数据库迁移记录失败: %w", err)
	}
	defer rows.Close()

	// 3. 逐条核对版本、dirty 状态和摘要。
	migrationCount := 0
	for rows.Next() {
		var version uint64
		var checksum string
		var dirty bool
		if err := rows.Scan(&version, &checksum, &dirty); err != nil {
			return fmt.Errorf("解析数据库迁移记录失败: %w", err)
		}
		migrationCount++
		if version != 1 {
			return fmt.Errorf("迁移版本 %d 与当前程序要求的版本 1 不兼容", version)
		}
		if dirty {
			return fmt.Errorf("迁移版本 1 的 dirty=true，请先检查未完成的迁移并修复")
		}
		if checksum != expectedChecksum {
			return fmt.Errorf("迁移版本 1 的摘要与程序内嵌 SQL 不一致，请核对迁移文件与数据库记录")
		}
	}

	// 4. 检查读取错误及记录缺失，全部通过后返回成功。
	if err := rows.Err(); err != nil {
		return fmt.Errorf("读取数据库迁移记录未完成: %w", err)
	}
	if migrationCount != 1 {
		return fmt.Errorf("迁移版本 1 的记录缺失，请先使用独立迁移命令初始化")
	}
	return nil
}
