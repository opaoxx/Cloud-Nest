package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/repository"
)

func TestServerBackupTargetB5RollbackSQLIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后验证 B5 回退库 SQL 导入")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	sample := prepareBackupTargetB5RollbackSample(t, ctx)
	backupDir := t.TempDir()
	manifest := exportBackupTargetB5RollbackBundle(t, ctx, sample, backupDir)
	restoreBackupTargetB5RollbackDatabase(t, ctx, sample, backupDir, manifest)
	t.Logf("本轮专用容器 MySQL %s，MinIO %s，后端已关停", sample.fixture.source.mysqlContainer, sample.fixture.source.minioContainer)
}

func restoreBackupTargetB5RollbackDatabase(t *testing.T, ctx context.Context, sample backupTargetB5RollbackSample,
	backupDir string, manifest backupSourceManifest,
) backupTargetDatabase {
	t.Helper()
	// 1. 核对新回退库与原始源、切换后样本属于三个独立实例。
	cutover, source := sample.fixture.database, sample.fixture.source
	target := prepareBackupTargetDatabase(t, ctx, cutover.containerID, cutover.name)
	if target.containerID == source.mysqlContainer || target.name == source.databaseName {
		t.Fatal("回退目标需要独立于原始源和切换后样本")
	}
	var sourceUUID, cutoverUUID, targetUUID string
	if err := source.pool.QueryRowContext(ctx, "SELECT @@GLOBAL.server_uuid").Scan(&sourceUUID); err != nil || sourceUUID == "" {
		t.Fatal("无法核对原始源 MySQL 实例", err)
	}
	if err := cutover.pool.QueryRowContext(ctx, "SELECT @@GLOBAL.server_uuid").Scan(&cutoverUUID); err != nil ||
		cutoverUUID == "" || cutoverUUID == sourceUUID {
		t.Fatal("切换后样本 MySQL 实例与原始源未分离", err)
	}
	if err := target.pool.QueryRowContext(ctx, "SELECT @@GLOBAL.server_uuid").Scan(&targetUUID); err != nil ||
		targetUUID == "" || targetUUID == sourceUUID || targetUUID == cutoverUUID {
		t.Fatal("回退目标 MySQL 实例与两个前置环境未分离", err)
	}

	// 2. 验证错误摘要不会导入，原样 SQL 只导入新空库一次。
	backupPath := filepath.Join(backupDir, "database.sql")
	if err := importBackupTargetSQL(ctx, target.pool, target.containerID, target.name, backupPath, strings.Repeat("0", 64)); err == nil ||
		!strings.Contains(err.Error(), "SQL 文件 SHA-256 与清单不符") {
		t.Fatal("回退库应拒绝不匹配的备份摘要", err)
	}
	if count, err := backupTargetTableCount(ctx, target.pool, target.name); err != nil || count != 0 {
		t.Fatalf("摘要不匹配后回退库应仍为空，表数 %d，错误 %v", count, err)
	}
	if err := importBackupTargetSQL(ctx, target.pool, target.containerID, target.name, backupPath, manifest.DatabaseSHA256); err != nil {
		t.Fatal("隔离回退库导入失败", err)
	}
	if err := importBackupTargetSQL(ctx, target.pool, target.containerID, target.name, backupPath, manifest.DatabaseSHA256); err == nil ||
		!strings.Contains(err.Error(), "目标库必须为空") {
		t.Fatal("回退库第二次导入应在空库检查处拒绝", err)
	}

	// 3. 在服务和后台任务未启动时，比对迁移、引用、日志及设备状态。
	if err := repository.CheckSchema(ctx, target.pool); err != nil {
		t.Fatal("回退库的迁移版本或摘要核对失败", err)
	}
	tableRows := verifyBackupTargetB5RollbackTableRows(t, ctx, target.pool)
	if count, err := backupTargetTableCount(ctx, target.pool, target.name); err != nil || count != len(tableRows) {
		t.Fatalf("回退库表数为 %d，预期 %d，错误 %v", count, len(tableRows), err)
	}
	restored, err := readBackupSourceManifest(ctx, target.pool, sample.fixture.targetBucket.name, manifest.DatabaseSHA256)
	if err != nil || !reflect.DeepEqual(restored, manifest) {
		t.Fatal("回退库的目录、当前 v3、两条历史引用或切换后对象定位与备份不符", err)
	}
	current := restored.FileReferences[0]
	state := readBackupB5DatabaseState(t, ctx, target.pool, current.UserID)
	if !reflect.DeepEqual(state, sample.state) {
		t.Fatal("回退库的账号序号、日志内容、配额或同步状态与停写样本不符")
	}
	for _, device := range state.devices {
		originalTimes := readBackupB5SyncDeviceTimes(t, ctx, cutover.pool, device.UserID, device.DeviceID)
		if readBackupB5SyncDeviceTimes(t, ctx, target.pool, device.UserID, device.DeviceID) != originalTimes {
			t.Fatal("回退库没有保留同步活动和创建设备记录的时间")
		}
	}
	if cutoverState := readBackupB5DatabaseState(t, ctx, cutover.pool, current.UserID); !reflect.DeepEqual(cutoverState, sample.state) {
		t.Fatal("回退库导入期间切换后样本的日志、配额或设备状态发生变化")
	}
	t.Logf("B5 回退库导入，独立 UUID %s，切换后 UUID %s，SQL SHA-256 %s，%d 张表，当前 v%d、历史 %d 条、日志 1～%d、已用 %d B、预占 %d B",
		targetUUID, cutoverUUID, manifest.DatabaseSHA256, len(tableRows), current.Revision, len(restored.HistoryReferences), state.high, state.used, state.reserved)
	return target
}
