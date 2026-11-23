package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestRestoreSyncCheckArguments(t *testing.T) {
	if err := run(t.Context(), []string{"--help"}); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("帮助应直接返回 %v", err)
	}
	if err := run(t.Context(), []string{"run"}); err == nil || err.Error() != usage {
		t.Fatalf("未知子命令应拒绝 %v", err)
	}
	t.Setenv("CLOUDNEST_RESTORE_DB_USER", "")
	t.Setenv("CLOUDNEST_DB_USER", "unused_test_user")
	args := []string{"check", "-db-address", "127.0.0.1:54123", "-database", "fixture",
		"-server-uuid", "4d835961-c21b-4910-8c42-0808c8f03362", "-roster", "fixture.json", "-mode", "initial"}
	if err := run(t.Context(), args); err == nil || !strings.Contains(err.Error(), "CLOUDNEST_RESTORE_DB_USER") {
		t.Fatalf("缺少恢复凭据时应在连接前拒绝 %v", err)
	}
}

func TestRestoreSyncCheckIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_RESTORE_SYNC_CHECK_TEST") != "1" {
		t.Skip("仅在专用 MySQL 中运行离线检查联测")
	}
	// 1. 在已由测试脚本核对的实例创建独占库和迁移记录。
	instanceUUID, err := uuid.Parse(os.Getenv("CLOUDNEST_RESTORE_SYNC_CHECK_UUID"))
	if err != nil || instanceUUID == uuid.Nil || os.Getenv("CLOUDNEST_DB_ADDRESS") == "" ||
		os.Getenv("CLOUDNEST_DB_NAME") != "information_schema" {
		t.Fatal("必须提供专用 MySQL 地址和已核对的实例 UUID")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	admin, err := config.LoadDatabaseConfig()
	if err != nil {
		t.Fatal(err)
	}
	adminPool, err := repository.OpenMySQL(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	defer adminPool.Close()
	var actualUUID string
	if err := adminPool.QueryRowContext(ctx, "SELECT @@GLOBAL.server_uuid").Scan(&actualUUID); err != nil || actualUUID != instanceUUID.String() {
		t.Fatalf("专用实例身份不符 %q，%v", actualUUID, err)
	}
	databaseName := fmt.Sprintf("cloudnest_restore_sync_check_%d", time.Now().UnixNano())
	if _, err := adminPool.ExecContext(ctx, "CREATE DATABASE `"+databaseName+"`"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanupContext, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, err := adminPool.ExecContext(cleanupContext, "DROP DATABASE `"+databaseName+"`"); err != nil {
			t.Errorf("清理独占库失败 %v", err)
		}
	}()
	target := admin
	target.DatabaseName = databaseName
	targetPool, err := repository.OpenMySQL(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	defer targetPool.Close()
	if _, err := targetPool.ExecContext(ctx, "CREATE TABLE schema_migrations (version BIGINT UNSIGNED PRIMARY KEY, checksum CHAR(64) NOT NULL, dirty BOOLEAN NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations.Ordered() {
		if _, err := targetPool.ExecContext(ctx, "INSERT INTO schema_migrations(version, checksum, dirty) VALUES (?, ?, FALSE)", migration.Version, migration.Checksum()); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CLOUDNEST_RESTORE_DB_USER", target.Username)
	t.Setenv("CLOUDNEST_RESTORE_DB_PASSWORD", target.Password)
	rosterPath := filepath.Join(t.TempDir(), "original-roster.json")
	check := func(serverUUID uuid.UUID) error {
		return run(ctx, []string{"check", "-db-address", target.Address, "-database", target.DatabaseName,
			"-server-uuid", serverUUID.String(), "-roster", rosterPath, "-mode", "resume"})
	}

	// 2. 正确目标通过，变更预期实例 UUID 时停止在身份校验。
	if err := check(instanceUUID); err != nil {
		t.Fatalf("正确目标和迁移应通过 %v", err)
	}
	wrongUUID := instanceUUID
	wrongUUID[15] ^= 1
	if err := check(wrongUUID); !errors.Is(err, repository.ErrRestoredSyncTargetMismatch) {
		t.Fatalf("错误实例应被拒绝 %v", err)
	}

	// 3. 制造未完成的迁移并确认检查失败，记录与清单路径保持原状。
	if _, err := targetPool.ExecContext(ctx, "UPDATE schema_migrations SET dirty=TRUE WHERE version=3"); err != nil {
		t.Fatal(err)
	}
	if err := check(instanceUUID); err == nil || !strings.Contains(err.Error(), "迁移版本 3 的 dirty=true") {
		t.Fatalf("未完成迁移应被拒绝 %v", err)
	}
	var dirty bool
	if err := targetPool.QueryRowContext(ctx, "SELECT dirty FROM schema_migrations WHERE version=3").Scan(&dirty); err != nil || !dirty {
		t.Fatalf("校验改变了迁移记录 %v，%v", dirty, err)
	}
	if _, err := os.Stat(rosterPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("校验应保持清单路径为空 %v", err)
	}
	t.Log("离线检查通过正确目标和完整迁移，拒绝错误实例及 dirty 迁移，未创建清单")
}
