package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type backupTargetDatabase struct {
	pool        *sql.DB
	containerID string
	name        string
}

func TestServerBackupTargetSQLIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后导入隔离目标库")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 重新生成已验证的 SQL 和清单，随后创建独立的目标 MySQL 空库。
	source := prepareBackupSource(t, ctx)
	verifyBackupSourceBaseline(t, ctx, source)
	backupDir := t.TempDir()
	dump := stopAndExportBackupSourceSQL(t, ctx, source, backupDir)
	manifest := writeAndVerifyBackupSourceManifest(t, ctx, source, backupDir, dump)
	target := prepareBackupTargetDatabase(t, ctx, source)

	// 2. 检查摘要错误会被拒绝，然后将原样 SQL 导入目标空库。
	backupPath := filepath.Join(backupDir, "database.sql")
	if err := importBackupTargetSQL(ctx, target.pool, target.containerID, target.name, backupPath, strings.Repeat("0", 64)); err == nil || !strings.Contains(err.Error(), "SQL 文件 SHA-256 与清单不符") {
		t.Fatalf("摘要错误未在导入前被拒绝：%v", err)
	}
	if err := importBackupTargetSQL(ctx, target.pool, target.containerID, target.name, backupPath, manifest.DatabaseSHA256); err != nil {
		t.Fatal(err)
	}
	if err := importBackupTargetSQL(ctx, target.pool, target.containerID, target.name, backupPath, manifest.DatabaseSHA256); err == nil || !strings.Contains(err.Error(), "目标库必须为空") {
		t.Fatalf("非空目标库未被拒绝：%v", err)
	}
	t.Logf("已校验目标容器内 SQL SHA-256 %s，摘要错误与重复导入均在写入前拒绝", manifest.DatabaseSHA256)

	// 3. 从目标库只读核对迁移、表行数、配额和完整目录与对象引用。
	userID := backupFixtureID(t, source.login.User.ID)
	tableRows := inspectBackupDatabase(t, ctx, target.pool, userID)
	verifyBackupSourceSQL(t, dump, tableRows)
	tableCount, err := backupTargetTableCount(ctx, target.pool, target.name)
	if err != nil || tableCount != len(tableRows) {
		t.Fatalf("目标库表数为 %d，预期 %d，错误 %v", tableCount, len(tableRows), err)
	}
	restored, err := readBackupSourceManifest(ctx, target.pool, source.bucket.name, manifest.DatabaseSHA256)
	if err != nil || !reflect.DeepEqual(restored, manifest) {
		t.Fatal("目标库的目录、文件引用或源对象定位与清单不符", err)
	}
	t.Logf("目标库核对通过：迁移历史完整，%d 张表，2 个目录、3 条文件引用及原对象版本，已用 10 B、预占 3 B", tableCount)
}

func prepareBackupTargetDatabase(t *testing.T, ctx context.Context, source backupSourceFixture) backupTargetDatabase {
	t.Helper()
	sourceAddress := os.Getenv("CLOUDNEST_DB_ADDRESS")
	containerID := startIsolatedWebMySQL(t, ctx)
	address := os.Getenv("CLOUDNEST_DB_ADDRESS")
	if containerID == source.mysqlContainer || address == sourceAddress {
		t.Fatal("恢复目标与源端未分离")
	}
	pool, name := prepareEmptyServerIntegrationDatabase(t, ctx)
	if name == source.databaseName {
		t.Fatal("恢复目标没有使用独立库名")
	}
	t.Logf("目标 MySQL 容器 %s，回环地址 %s，独占库 %s", containerID, address, name)
	return backupTargetDatabase{pool: pool, containerID: containerID, name: name}
}

func importBackupTargetSQL(ctx context.Context, target *sql.DB, containerID, databaseName, backupPath, expectedDigest string) error {
	// 1. 从目标连接复核库名和空库状态，再检查备份文件摘要。
	count, err := backupTargetTableCount(ctx, target, databaseName)
	if err != nil {
		return err
	}
	if count != 0 {
		return errors.New("目标库必须为空")
	}
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return fmt.Errorf("读取 SQL 文件失败: %w", err)
	}
	if fmt.Sprintf("%x", sha256.Sum256(data)) != expectedDigest {
		return errors.New("SQL 文件 SHA-256 与清单不符")
	}

	// 2. 原样复制 SQL，复核容器内文件的摘要后才执行导入。
	const containerPath = "/tmp/cloudnest-restore.sql"
	if output, err := exec.CommandContext(ctx, "docker", "cp", backupPath, containerID+":"+containerPath).CombinedOutput(); err != nil {
		return fmt.Errorf("复制目标库 SQL 文件失败: %w\n%s", err, output)
	}
	if err := verifyBackupSQLContainerDigest(ctx, containerID, containerPath, expectedDigest); err != nil {
		return err
	}
	restore := exec.CommandContext(ctx, "docker", "exec", "--env", "MYSQL_PWD", containerID, "mysql", "--user=root",
		"--database="+databaseName, "--execute=source "+containerPath)
	restore.Env = append(restore.Environ(), "MYSQL_PWD="+os.Getenv("CLOUDNEST_DB_PASSWORD"))
	if output, err := restore.CombinedOutput(); err != nil {
		return fmt.Errorf("隔离目标库导入失败: %w\n%s", err, output)
	}
	return nil
}

func backupTargetTableCount(ctx context.Context, target *sql.DB, databaseName string) (int, error) {
	var currentDatabase string
	var count int
	if err := target.QueryRowContext(ctx, `SELECT DATABASE(),
		(SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE())`).Scan(&currentDatabase, &count); err != nil {
		return 0, fmt.Errorf("读取目标库结构失败: %w", err)
	}
	if currentDatabase != databaseName {
		return 0, errors.New("目标连接与指定库名不符")
	}
	return count, nil
}
