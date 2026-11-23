package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestServerBackupTargetB5SyncResetIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后验证已恢复目标的离线同步换代")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 核对已恢复目标的身份和换代前状态。
	fixture := prepareBackupTargetB5Remap(t, ctx)
	verifyAndApplyBackupTargetB5ObjectMap(t, ctx, fixture)
	resetBackupTargetB5SyncDevices(t, ctx, fixture)
	t.Logf("本轮专用容器 MySQL %s，MinIO %s，后端已关停", fixture.source.mysqlContainer, fixture.source.minioContainer)
}

func resetBackupTargetB5SyncDevices(t *testing.T, ctx context.Context, fixture backupTargetRemapFixture) {
	t.Helper()
	userID := backupFixtureID(t, fixture.source.login.User.ID)
	before := readBackupB5DatabaseState(t, ctx, fixture.database.pool, userID)
	sourceBefore := readBackupB5DatabaseState(t, ctx, fixture.source.pool, userID)
	if len(before.devices) != 1 || before.devices[0].UserID != userID ||
		before.devices[0].State.Status != domain.SyncDeviceStatusActive || before.devices[0].State.AckSequence != 2 {
		t.Fatal("恢复目标在换代前应保留活动设备及确认位置 2")
	}
	address, instanceUUID, rosterDigest := verifyBackupB5OfflineSyncReset(t, ctx, fixture.database, fixture.targetBucket.name, fixture.manifest, before)
	if source := readBackupB5DatabaseState(t, ctx, fixture.source.pool, userID); !reflect.DeepEqual(source, sourceBefore) {
		t.Fatal("目标换代后停写的源库状态不符")
	}
	t.Logf("B5 目标 %s UUID %s 离线换代，提交 1 条并复核 1 条，设备 %d 待重建、确认位置 0、新代次有效，原清单 SHA-256 %x",
		address, instanceUUID, before.devices[0].DeviceID, rosterDigest)
}

func verifyBackupB5OfflineSyncReset(t *testing.T, ctx context.Context, target backupTargetDatabase, bucket string,
	manifest backupSourceManifest, before backupB5DatabaseState,
) (address, instanceUUID string, rosterDigest [sha256.Size]byte) {
	t.Helper()
	// 1. 校对专用目标的实例、回环端口和换代前的数据基线。
	if len(before.devices) != 1 || !before.devices[0].State.IsValid() {
		t.Fatal("离线换代夹具需要一个有效的同步设备记录")
	}
	var targetUUID, targetName string
	if err := target.pool.QueryRowContext(ctx, "SELECT @@GLOBAL.server_uuid, DATABASE()").Scan(&targetUUID, &targetName); err != nil ||
		targetUUID == "" || targetName != target.name {
		t.Fatal("隔离目标实例和库名核对失败", err)
	}
	containerAddress, err := exec.CommandContext(ctx, "docker", "port", target.containerID, "3306/tcp").Output()
	targetAddress := strings.TrimSpace(string(containerAddress))
	user := os.Getenv("CLOUDNEST_DB_USER")
	password, passwordSet := os.LookupEnv("CLOUDNEST_DB_PASSWORD")
	if err != nil || !strings.HasPrefix(targetAddress, "127.0.0.1:") || targetAddress != os.Getenv("CLOUDNEST_DB_ADDRESS") ||
		user == "" || !passwordSet || password == "" {
		t.Fatal("离线命令的地址或凭据未明确指向本轮专用目标", err)
	}
	userID, deviceID := before.devices[0].UserID, before.devices[0].DeviceID
	timesBefore := readBackupB5SyncDeviceTimes(t, ctx, target.pool, userID, deviceID)
	objectsBefore := snapshotBackupTargetObjectsWithCount(t, ctx, target.pool, userID, len(manifest.Objects))
	manifestBefore, err := readBackupSourceManifest(ctx, target.pool, bucket, manifest.DatabaseSHA256)
	if err != nil {
		t.Fatal("读取已对齐的目标引用失败", err)
	}

	// 2. 通过命令预检并执行首次换代。
	rosterPath := filepath.Join(t.TempDir(), "original-sync-roster.json")
	runRestore := func(action string) string {
		t.Helper()
		command := exec.CommandContext(ctx, "go", "run", "../restore-sync", action,
			"-db-address", targetAddress, "-database", targetName, "-server-uuid", targetUUID, "-roster", rosterPath, "-mode", "initial")
		command.Env = append(command.Environ(), "CLOUDNEST_RESTORE_DB_USER="+user, "CLOUDNEST_RESTORE_DB_PASSWORD="+password)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("隔离目标离线命令 %s 失败 %v\n%s", action, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	if output := runRestore("check"); output != "目标实例、库名和迁移记录校验通过" {
		t.Fatalf("离线预检结果不符 %q", output)
	}
	if _, err := os.Stat(rosterPath); !os.IsNotExist(err) || !reflect.DeepEqual(readBackupB5DatabaseState(t, ctx, target.pool, userID), before) {
		t.Fatal("预检修改了设备、业务状态或清单路径", err)
	}
	if output := runRestore("reset"); output != "同步换代完成，已提交 1 条，复核通过 1 条" {
		t.Fatalf("离线换代的提交和复核结果不符 %q", output)
	}

	// 3. 从清单和目标库独立读回，核对换代结果。
	original, err := service.LoadRestoredSyncDeviceRoster(rosterPath)
	if err != nil || !slices.Equal(original, before.devices) {
		t.Fatal("首次清单未保留导入时的完整代次和确认位置", err)
	}
	after := readBackupB5DatabaseState(t, ctx, target.pool, userID)
	if len(after.devices) != 1 || after.devices[0].UserID != userID || after.devices[0].DeviceID != deviceID ||
		after.devices[0].State.Status != domain.SyncDeviceStatusResetRequired || after.devices[0].State.AckSequence != 0 ||
		!after.devices[0].State.IsValid() || after.devices[0].State.Generation == original[0].State.Generation {
		t.Fatal("目标设备未读回新代次、待重建状态和零确认位置")
	}
	expected := before
	expected.devices = after.devices
	manifestAfter, err := readBackupSourceManifest(ctx, target.pool, bucket, manifest.DatabaseSHA256)
	if err != nil || !reflect.DeepEqual(manifestAfter, manifestBefore) || !reflect.DeepEqual(after, expected) ||
		!reflect.DeepEqual(snapshotBackupTargetObjectsWithCount(t, ctx, target.pool, userID, len(manifest.Objects)), objectsBefore) ||
		readBackupB5SyncDeviceTimes(t, ctx, target.pool, userID, deviceID) != timesBefore {
		t.Fatal("离线换代改变了同步活动时间、目录、日志、对象引用或配额", err)
	}
	roster, err := os.ReadFile(rosterPath)
	if err != nil {
		t.Fatal(err)
	}
	return targetAddress, targetUUID, sha256.Sum256(roster)
}

type backupB5SyncDeviceTimes struct {
	lastSeen, created time.Time
}

func readBackupB5SyncDeviceTimes(t *testing.T, ctx context.Context, pool *sql.DB, userID, deviceID uint64) backupB5SyncDeviceTimes {
	t.Helper()
	var times backupB5SyncDeviceTimes
	if err := pool.QueryRowContext(ctx, "SELECT last_seen_at,created_at FROM sync_devices WHERE user_id=? AND device_id=?", userID, deviceID).
		Scan(&times.lastSeen, &times.created); err != nil {
		t.Fatal("读取设备同步活动与创建时间失败", err)
	}
	return times
}
