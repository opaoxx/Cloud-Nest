package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/migrations"
)

// schemaCheckDatabase 仅在显式启用时创建本用例独占的测试库，并登记删除操作。
func schemaCheckDatabase(t *testing.T) *sql.DB {
	t.Helper()
	if os.Getenv("CLOUDNEST_SCHEMA_CHECK_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_SCHEMA_CHECK_TEST=1 后执行自动创建和清理隔离库的迁移检查测试")
	}
	databaseConfig, err := config.LoadDatabaseConfig()
	if err != nil {
		t.Fatal(err)
	}
	adminPool, err := OpenMySQL(context.Background(), databaseConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adminPool.Close() })

	// 1. 库名只由固定前缀与时间数字生成，创建失败时不登记删除，避免删除已有库。
	databaseName := fmt.Sprintf("cloudnest_schema_check_%d", time.Now().UnixNano())
	createContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := adminPool.ExecContext(createContext, "CREATE DATABASE `"+databaseName+"`"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := adminPool.ExecContext(cleanupContext, "DROP DATABASE `"+databaseName+"`"); err != nil {
			t.Errorf("清理隔离测试库 %s 失败: %v", databaseName, err)
		}
	})

	// 2. 返回只连接该新库的池；清理按后登记先执行，先关闭池再删除库。
	databaseConfig.DatabaseName = databaseName
	connectionPool, err := OpenMySQL(context.Background(), databaseConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connectionPool.Close() })
	return connectionPool
}

func TestCheckSchemaIntegration(t *testing.T) {
	// 1. 在独占测试库构造迁移记录；固定摘要对应已应用且不允许改写的版本 1 SQL。
	connectionPool := schemaCheckDatabase(t)
	const validChecksum = "627a990d6b3e5cf77b3e01ff468806cfbb77da42a96bb46f0162cfedc0a4cfc7"
	type migrationRecord struct {
		version  uint64
		checksum string
		dirty    bool
	}
	validRecord := migrationRecord{version: 1, checksum: validChecksum}
	catalog := migrations.Ordered()
	versionTwo := migrationRecord{version: 2, checksum: catalog[1].Checksum()}
	versionThree := migrationRecord{version: 3, checksum: catalog[2].Checksum()}
	testCases := []struct {
		name      string
		noTable   bool
		records   []migrationRecord
		wantError string
	}{
		{name: "missing table", noTable: true, wantError: "缺少 schema_migrations"},
		{name: "empty ledger", wantError: "记录缺失"},
		{name: "valid schema", records: []migrationRecord{validRecord, versionTwo, versionThree}},
		{name: "missing second migration", records: []migrationRecord{validRecord}, wantError: "迁移版本 2 的记录缺失"},
		{name: "missing sync migration", records: []migrationRecord{validRecord, versionTwo}, wantError: "迁移版本 3 的记录缺失"},
		{name: "older version", records: []migrationRecord{{version: 0, checksum: validChecksum}}, wantError: "迁移版本 0"},
		{name: "missing first migration", records: []migrationRecord{versionTwo}, wantError: "迁移版本 2"},
		{name: "extra future version", records: []migrationRecord{validRecord, versionTwo, versionThree, {version: 4, checksum: validChecksum}}, wantError: "迁移版本 4"},
		{name: "dirty migration", records: []migrationRecord{{version: 1, checksum: validChecksum, dirty: true}}, wantError: "dirty=true"},
		{name: "checksum mismatch", records: []migrationRecord{{version: 1, checksum: strings.Repeat("0", 64)}}, wantError: "摘要"},
		{name: "dirty second migration", records: []migrationRecord{validRecord, {version: 2, checksum: versionTwo.checksum, dirty: true}}, wantError: "迁移版本 2 的 dirty=true"},
		{name: "second checksum mismatch", records: []migrationRecord{validRecord, {version: 2, checksum: strings.Repeat("0", 64)}}, wantError: "迁移版本 2 的摘要"},
		{name: "dirty sync migration", records: []migrationRecord{validRecord, versionTwo, {version: 3, checksum: versionThree.checksum, dirty: true}}, wantError: "迁移版本 3 的 dirty=true"},
		{name: "sync checksum mismatch", records: []migrationRecord{validRecord, versionTwo, {version: 3, checksum: strings.Repeat("0", 64)}}, wantError: "迁移版本 3 的摘要"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			fixtureContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := connectionPool.ExecContext(fixtureContext, "DROP TABLE IF EXISTS schema_migrations"); err != nil {
				t.Fatal(err)
			}
			if !testCase.noTable {
				if _, err := connectionPool.ExecContext(fixtureContext, "CREATE TABLE schema_migrations (version BIGINT UNSIGNED PRIMARY KEY, checksum CHAR(64) NOT NULL, dirty BOOLEAN NOT NULL)"); err != nil {
					t.Fatal(err)
				}
				for _, record := range testCase.records {
					if _, err := connectionPool.ExecContext(fixtureContext, "INSERT INTO schema_migrations(version,checksum,dirty) VALUES(?,?,?)", record.version, record.checksum, record.dirty); err != nil {
						t.Fatal(err)
					}
				}
			}

			// 2. 调用真实查询入口，正常记录通过，异常记录必须说明原因。
			err := CheckSchema(fixtureContext, connectionPool)
			if testCase.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), testCase.wantError) {
				t.Fatalf("预期错误包含 %q，实际为 %v", testCase.wantError, err)
			}
		})
	}

	// 3. 提前取消应直接失败；占满池时，检查必须受调用方及默认期限约束。
	t.Run("canceled context", func(t *testing.T) {
		checkContext, cancel := context.WithCancel(context.Background())
		cancel()
		if err := CheckSchema(checkContext, connectionPool); !errors.Is(err, context.Canceled) {
			t.Fatalf("预期取消错误，实际为 %v", err)
		}
	})
	connectionPool.SetMaxOpenConns(1)
	reserveContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reservedConnection, err := connectionPool.Conn(reserveContext)
	if err != nil {
		t.Fatal(err)
	}
	defer reservedConnection.Close()
	for _, callerTimeout := range []time.Duration{100 * time.Millisecond, 10 * time.Second} {
		t.Run(fmt.Sprintf("pool wait with caller timeout %s", callerTimeout), func(t *testing.T) {
			checkContext, cancel := context.WithTimeout(context.Background(), callerTimeout)
			defer cancel()
			startedAt := time.Now()
			err := CheckSchema(checkContext, connectionPool)
			elapsed := time.Since(startedAt)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("预期期限错误，实际为 %v", err)
			}
			if callerTimeout == 100*time.Millisecond && elapsed > 2*time.Second {
				t.Fatalf("调用方的更短期限未及时生效，实际等待 %s", elapsed)
			}
			if callerTimeout == 10*time.Second && (elapsed < 4*time.Second || elapsed > 8*time.Second) {
				t.Fatalf("默认 5 秒期限未生效，实际等待 %s", elapsed)
			}
		})
	}
}
