package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
)

func TestServerBackupTargetB5SQLIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后导入隔离 B5 目标库")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 生成已核验的停写 SQL，并建立独立目标空库。
	source := prepareBackupSource(t, ctx)
	backupDir := t.TempDir()
	manifest := prepareBackupSourceB5Manifest(t, ctx, source, backupDir)
	backupState := readBackupB5DatabaseState(t, ctx, source.pool, backupFixtureID(t, source.login.User.ID))
	restoreBackupTargetB5Database(t, ctx, source, backupDir, manifest, backupState)
	t.Logf("本轮专用容器 MySQL %s，MinIO %s，后端已关停", source.mysqlContainer, source.minioContainer)
}

func restoreBackupTargetB5Database(t *testing.T, ctx context.Context, source backupSourceFixture,
	backupDir string, manifest backupSourceManifest, backupState backupB5DatabaseState,
) backupTargetDatabase {
	t.Helper()
	target := prepareBackupTargetDatabase(t, ctx, source.mysqlContainer, source.databaseName)
	var sourceUUID, targetUUID string
	if err := source.pool.QueryRowContext(ctx, "SELECT @@GLOBAL.server_uuid").Scan(&sourceUUID); err != nil || sourceUUID == "" {
		t.Fatal("无法核对隔离源 MySQL 实例", err)
	}
	if err := target.pool.QueryRowContext(ctx, "SELECT @@GLOBAL.server_uuid").Scan(&targetUUID); err != nil || targetUUID == "" || targetUUID == sourceUUID {
		t.Fatal("目标 MySQL 实例与源端未分离", err)
	}

	// 2. 校验文件摘要，导入后拒绝重复导入。
	backupPath := filepath.Join(backupDir, "database.sql")
	if err := importBackupTargetSQL(ctx, target.pool, target.containerID, target.name, backupPath, strings.Repeat("0", 64)); err == nil ||
		!strings.Contains(err.Error(), "SQL 文件 SHA-256 与清单不符") {
		t.Fatalf("错误摘要应在目标导入前拒绝 %v", err)
	}
	if count, err := backupTargetTableCount(ctx, target.pool, target.name); err != nil || count != 0 {
		t.Fatalf("摘要校验失败后目标库应为空，实际表数 %d，错误 %v", count, err)
	}
	if err := importBackupTargetSQL(ctx, target.pool, target.containerID, target.name, backupPath, manifest.DatabaseSHA256); err != nil {
		t.Fatal("B5 目标 SQL 导入失败", err)
	}
	if err := importBackupTargetSQL(ctx, target.pool, target.containerID, target.name, backupPath, manifest.DatabaseSHA256); err == nil ||
		!strings.Contains(err.Error(), "目标库必须为空") {
		t.Fatalf("重复导入应在非空库检查处被拒绝 %v", err)
	}

	// 3. 只读比较目标数据和源端停写记录。
	if err := repository.CheckSchema(ctx, target.pool); err != nil {
		t.Fatal("B5 目标库迁移记录核对失败", err)
	}
	if count, err := backupTargetTableCount(ctx, target.pool, target.name); err != nil || count != 13 {
		t.Fatalf("B5 目标库表数为 %d，错误 %v", count, err)
	}
	restored, err := readBackupSourceManifest(ctx, target.pool, source.bucket.name, manifest.DatabaseSHA256)
	if err != nil || !reflect.DeepEqual(restored, manifest) {
		t.Fatal("B5 目标库的目录、当前和历史引用或原对象定位与清单不符", err)
	}
	userID := backupFixtureID(t, source.login.User.ID)
	deviceID := backupFixtureID(t, source.login.Device.ID)
	targetState := readBackupB5DatabaseState(t, ctx, target.pool, userID)
	if !reflect.DeepEqual(targetState, backupState) || targetState.high != 4 || targetState.used != 72 || targetState.reserved != 0 ||
		len(targetState.devices) != 1 || targetState.devices[0].UserID != userID || targetState.devices[0].DeviceID != deviceID ||
		targetState.devices[0].State.Status != domain.SyncDeviceStatusActive || targetState.devices[0].State.AckSequence != 2 ||
		len(targetState.events) != 4 || targetState.events[0].sequence != 1 || targetState.events[3].sequence != 4 {
		t.Fatal("目标库的账号配额、设备原代次或保留日志与停写源库不符")
	}
	t.Logf("B5 目标导入已核对，目标 UUID %s，SQL SHA-256 %s，13 张表，设备原代次及确认位置 2，日志 1～4，当前和历史引用一致，已用 72 B",
		targetUUID, manifest.DatabaseSHA256)
	return target
}

type backupB5DatabaseEvent struct {
	sequence, entryID, revision uint64
	eventType, payload          string
	createdAt                   time.Time
}

type backupB5DatabaseState struct {
	high, used, reserved uint64
	devices              []domain.RestoredSyncDeviceCandidate
	events               []backupB5DatabaseEvent
}

func readBackupB5DatabaseState(t *testing.T, ctx context.Context, pool *sql.DB, userID uint64) backupB5DatabaseState {
	t.Helper()
	// 1. 读取账号账目和包含完整代次的同步设备。
	var state backupB5DatabaseState
	if err := pool.QueryRowContext(ctx, "SELECT change_seq,used_storage,reserved_storage FROM users WHERE id=?", userID).
		Scan(&state.high, &state.used, &state.reserved); err != nil {
		t.Fatal("读取隔离库账号状态失败", err)
	}
	store, err := repository.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	state.devices, err = store.ListRestoredSyncDeviceCandidates(ctx, 0, 2)
	if err != nil {
		t.Fatal("读取隔离库的同步设备失败", err)
	}

	// 2. 顺序读回事件完整内容，供停写源库和目标库逐条比较。
	rows, err := pool.QueryContext(ctx, "SELECT `sequence`,entry_id,revision,event_type,payload,created_at FROM change_events WHERE user_id=? ORDER BY `sequence`", userID)
	if err != nil {
		t.Fatal("读取隔离库的变更日志失败", err)
	}
	defer rows.Close()
	for rows.Next() {
		var event backupB5DatabaseEvent
		if err := rows.Scan(&event.sequence, &event.entryID, &event.revision, &event.eventType, &event.payload, &event.createdAt); err != nil {
			t.Fatal("读取隔离库的变更记录失败", err)
		}
		state.events = append(state.events, event)
	}
	if err := rows.Err(); err != nil {
		t.Fatal("变更记录未读完", err)
	}
	return state
}
