package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestRestoreSyncCheckArguments(t *testing.T) {
	if err := run(t.Context(), []string{"--help"}, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("帮助应直接返回 %v", err)
	}
	if err := run(t.Context(), []string{"run"}, io.Discard); err == nil || err.Error() != usage {
		t.Fatalf("未知子命令应拒绝 %v", err)
	}
	t.Setenv("CLOUDNEST_RESTORE_DB_USER", "")
	t.Setenv("CLOUDNEST_DB_USER", "unused_test_user")
	args := []string{"check", "-db-address", "127.0.0.1:54123", "-database", "fixture",
		"-server-uuid", "4d835961-c21b-4910-8c42-0808c8f03362", "-roster", "fixture.json", "-mode", "initial"}
	if err := run(t.Context(), args, io.Discard); err == nil || !strings.Contains(err.Error(), "CLOUDNEST_RESTORE_DB_USER") {
		t.Fatalf("缺少恢复凭据时应在连接前拒绝 %v", err)
	}
}

func prepareRestoreSyncCommandDatabase(t *testing.T, ctx context.Context) (config.DatabaseConfig, *sql.DB, uuid.UUID) {
	t.Helper()
	if os.Getenv("CLOUDNEST_RESTORE_SYNC_CHECK_TEST") != "1" {
		t.Skip("仅在专用 MySQL 中运行离线恢复命令联测")
	}
	// 1. 在已由测试脚本核对的实例创建独占库和迁移记录。
	instanceUUID, err := uuid.Parse(os.Getenv("CLOUDNEST_RESTORE_SYNC_CHECK_UUID"))
	if err != nil || instanceUUID == uuid.Nil || os.Getenv("CLOUDNEST_DB_ADDRESS") == "" ||
		os.Getenv("CLOUDNEST_DB_NAME") != "information_schema" {
		t.Fatal("必须提供专用 MySQL 地址和已核对的实例 UUID")
	}
	admin, err := config.LoadDatabaseConfig()
	if err != nil {
		t.Fatal(err)
	}
	adminPool, err := repository.OpenMySQL(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adminPool.Close() })
	var actualUUID string
	if err := adminPool.QueryRowContext(ctx, "SELECT @@GLOBAL.server_uuid").Scan(&actualUUID); err != nil || actualUUID != instanceUUID.String() {
		t.Fatalf("专用实例身份不符 %q，%v", actualUUID, err)
	}
	databaseName := fmt.Sprintf("cloudnest_restore_sync_check_%d", time.Now().UnixNano())
	if _, err := adminPool.ExecContext(ctx, "CREATE DATABASE `"+databaseName+"`"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupContext, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, err := adminPool.ExecContext(cleanupContext, "DROP DATABASE `"+databaseName+"`"); err != nil {
			t.Errorf("清理独占库失败 %v", err)
		}
	})
	target := admin
	target.DatabaseName = databaseName
	targetPool, err := repository.OpenMySQL(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = targetPool.Close() })
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
	return target, targetPool, instanceUUID
}

func TestRestoreSyncCheckIntegration(t *testing.T) {
	// 1. 准备已核对的目标和迁移记录。
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	target, targetPool, instanceUUID := prepareRestoreSyncCommandDatabase(t, ctx)
	rosterPath := filepath.Join(t.TempDir(), "original-roster.json")
	check := func(serverUUID uuid.UUID) error {
		return run(ctx, []string{"check", "-db-address", target.Address, "-database", target.DatabaseName,
			"-server-uuid", serverUUID.String(), "-roster", rosterPath, "-mode", "resume"}, io.Discard)
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

func TestRestoreSyncResetIntegration(t *testing.T) {
	// 1. 准备两台合成设备和原始清单路径，只在已核对的独占库中构造故障。
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	target, targetPool, instanceUUID := prepareRestoreSyncCommandDatabase(t, ctx)
	accountDDL, _, foundAccounts := strings.Cut(migrations.InitialSQL, "CREATE TABLE auth_sessions (")
	_, syncDDL, foundSync := strings.Cut(migrations.Ordered()[2].SQL, ";\n")
	if !foundAccounts || !foundSync {
		t.Fatal("未找到合成夹具所需的迁移表定义")
	}
	execute := func(statement string) {
		t.Helper()
		if _, err := targetPool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range strings.Split(accountDDL, ";\n") {
		if strings.TrimSpace(statement) != "" {
			execute(statement)
		}
	}
	execute(syncDDL)
	execute("INSERT INTO users(id,username,display_name,password_hash,change_seq) VALUES(1,'restore_fixture','恢复夹具','fixture_hash',7)")
	execute(`INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES
		(19,1,'4d835961-c21b-4910-8c42-0808c8f03361','活动设备','WEB'),
		(20,1,'4d835961-c21b-4910-8c42-0808c8f03362','待重建设备','WEB')`)
	execute(`INSERT INTO sync_devices(device_id,user_id,status,cursor_generation,ack_sequence,last_seen_at) VALUES
		(19,1,'ACTIVE',UNHEX(REPEAT('11',16)),7,UTC_TIMESTAMP(6)),
		(20,1,'RESET_REQUIRED',UNHEX(REPEAT('22',16)),0,UTC_TIMESTAMP(6))`)
	store, err := repository.NewStore(targetPool)
	if err != nil {
		t.Fatal(err)
	}
	readDevices := func() []domain.RestoredSyncDeviceCandidate {
		t.Helper()
		devices, err := store.ListRestoredSyncDeviceCandidates(ctx, 0, 3)
		if err != nil || len(devices) != 2 {
			t.Fatalf("未读到两条设备记录 %+v，%v", devices, err)
		}
		return devices
	}
	rosterPath := filepath.Join(t.TempDir(), "original-roster.json")
	var output bytes.Buffer
	reset := func(serverUUID uuid.UUID, mode string) error {
		output.Reset()
		return run(ctx, []string{"reset", "-db-address", target.Address, "-database", target.DatabaseName,
			"-server-uuid", serverUUID.String(), "-roster", rosterPath, "-mode", mode}, &output)
	}
	before := readDevices()
	wrongUUID := instanceUUID
	wrongUUID[15] ^= 1
	if err := reset(wrongUUID, "initial"); !errors.Is(err, repository.ErrRestoredSyncTargetMismatch) || output.Len() != 0 {
		t.Fatalf("目标核对失败时应停止 %v，%q", err, output.String())
	}
	if err := reset(instanceUUID, "resume"); !errors.Is(err, os.ErrNotExist) || output.Len() != 0 || !slices.Equal(readDevices(), before) {
		t.Fatalf("缺少原清单时应保持设备状态 %v，%q", err, output.String())
	}

	// 2. 首次保存后让第二台换代失败，重复首次运行应保留同一清单和已提交状态。
	execute(`CREATE TRIGGER fixture_restore_sync_reset_failure BEFORE UPDATE ON sync_devices FOR EACH ROW
		BEGIN IF OLD.device_id=20 THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='fixture reset failure'; END IF; END`)
	var databaseError *mysql.MySQLError
	if err := reset(instanceUUID, "initial"); !errors.As(err, &databaseError) || databaseError.Number != 1644 ||
		!strings.Contains(err.Error(), "已提交 1 条，复核通过 0 条") || output.Len() != 0 {
		t.Fatalf("首次部分提交应报告失败进度 %v，%q", err, output.String())
	}
	partial := readDevices()
	saved, err := service.LoadRestoredSyncDeviceRoster(rosterPath)
	if err != nil || !slices.Equal(saved, before) || partial[0].State.Generation == before[0].State.Generation || partial[1] != before[1] {
		t.Fatalf("原清单或失败后状态不符 %+v，%+v，%v", saved, partial, err)
	}
	originalBytes, err := os.ReadFile(rosterPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := reset(instanceUUID, "initial"); !errors.Is(err, os.ErrExist) || output.Len() != 0 || !slices.Equal(readDevices(), partial) {
		t.Fatalf("重复首次运行应退出且保持已提交状态 %v，%q", err, output.String())
	}

	// 3. 移除夹具故障后用原清单重跑，核对提交、复核和原文件。
	execute("DROP TRIGGER fixture_restore_sync_reset_failure")
	if err := reset(instanceUUID, "resume"); err != nil || output.String() != "同步换代完成，已提交 2 条，复核通过 2 条\n" {
		t.Fatalf("重跑应报告全部提交并复核 %v，%q", err, output.String())
	}
	for index, device := range readDevices() {
		if device.State.Status != domain.SyncDeviceStatusResetRequired || device.State.AckSequence != 0 ||
			device.State.Generation == before[index].State.Generation || device.State.Generation == partial[index].State.Generation {
			t.Fatalf("设备 %d 换代结果不符 %+v", device.DeviceID, device.State)
		}
	}
	if currentBytes, err := os.ReadFile(rosterPath); err != nil || !bytes.Equal(currentBytes, originalBytes) {
		t.Fatalf("拒绝重复首次运行和重跑后应保留原清单字节 %v", err)
	}
	t.Log("首次失败前确认提交 1 条，重复首次运行被拒，使用原清单重跑后 2 条均提交且复核通过")
}
