package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrRestoredSyncTargetMismatch 表示实际连接的实例或库与预先记录的恢复目标不符。
var ErrRestoredSyncTargetMismatch = errors.New("MySQL 连接与记录的恢复目标不符")

// CheckRestoredSyncTarget 只读核对指定连接的身份，连接仍由调用方管理。
// 预期值来自隔离恢复记录。调用方负责保持目标停服，并在连接更换后重新核对。
func CheckRestoredSyncTarget(ctx context.Context, connection *sql.Conn, expectedServerUUID uuid.UUID, expectedDatabase string) error {
	// 1. 校验预期实例与库名，并为查询设置期限。
	if ctx == nil || connection == nil || expectedServerUUID == uuid.Nil ||
		expectedDatabase == "" || expectedDatabase != strings.TrimSpace(expectedDatabase) {
		return fmt.Errorf("核对同步恢复目标需要上下文、连接、非零实例 UUID 和明确的库名")
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return err
	}

	// 2. 从同一条连接读取实例 UUID 和当前库名。
	var serverUUIDText string
	var databaseName sql.NullString
	if err := connection.QueryRowContext(queryContext, "SELECT @@GLOBAL.server_uuid, DATABASE()").Scan(&serverUUIDText, &databaseName); err != nil {
		return fmt.Errorf("读取 MySQL 恢复目标身份失败: %w", err)
	}
	serverUUID, err := uuid.Parse(serverUUIDText)
	if err != nil || serverUUID == uuid.Nil {
		return fmt.Errorf("MySQL 恢复目标返回无效的实例 UUID")
	}
	if err := queryContext.Err(); err != nil {
		return err
	}

	// 3. 两项都匹配才返回成功。
	if !databaseName.Valid || databaseName.String != expectedDatabase || serverUUID != expectedServerUUID {
		return fmt.Errorf("%w（预期实例 %s、库 %q，实际实例 %s、库 %q）",
			ErrRestoredSyncTargetMismatch, expectedServerUUID, expectedDatabase, serverUUID, databaseName.String)
	}
	return nil
}
