package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/migrations"
)

// CheckSchema 在最多 5 秒内只读核对迁移记录是否符合当前程序预期。
// 所有已记录版本必须与内嵌清单一致且为 clean 状态。本方法不执行迁移或修复。
// 检查不验证实际表结构，也不代替迁移期间的停机维护流程。
func CheckSchema(ctx context.Context, connection interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) error {
	// 1. 设置检查期限，并取得内嵌文件对应的有序迁移清单。
	checkContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	expectedMigrations := migrations.Ordered()

	// 2. 按版本读取，最多多读一条以识别清单之外的记录。
	rows, err := connection.QueryContext(checkContext,
		"SELECT version, checksum, dirty FROM schema_migrations ORDER BY version LIMIT ?", len(expectedMigrations)+1)
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
		if migrationCount == len(expectedMigrations) {
			return fmt.Errorf("迁移版本 %d 超出当前程序支持的迁移清单", version)
		}
		expected := expectedMigrations[migrationCount]
		if version != expected.Version {
			return fmt.Errorf("迁移版本 %d 与当前程序要求的版本 %d 不兼容", version, expected.Version)
		}
		if dirty {
			return fmt.Errorf("迁移版本 %d 的 dirty=true，请先检查未完成的迁移并修复", version)
		}
		if checksum != expected.Checksum() {
			return fmt.Errorf("迁移版本 %d 的摘要与程序内嵌 SQL 不一致，请核对迁移文件与数据库记录", version)
		}
		migrationCount++
	}

	// 4. 检查读取错误及记录缺失，全部通过后返回成功。
	if err := rows.Err(); err != nil {
		return fmt.Errorf("读取数据库迁移记录未完成: %w", err)
	}
	if migrationCount < len(expectedMigrations) {
		return fmt.Errorf("迁移版本 %d 的记录缺失，请先使用独立迁移命令初始化", expectedMigrations[migrationCount].Version)
	}
	return nil
}
