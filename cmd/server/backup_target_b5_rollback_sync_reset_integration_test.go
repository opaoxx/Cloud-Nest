package main

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestServerBackupTargetB5RollbackSyncResetIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后验证 B5 回退库离线同步换代")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 从同一轮备份重建停服回退库，并核对三个对象的新定位。
	sample := prepareBackupTargetB5RollbackSample(t, ctx)
	backupDir := t.TempDir()
	manifest := exportBackupTargetB5RollbackBundle(t, ctx, sample, backupDir)
	database := restoreBackupTargetB5RollbackDatabase(t, ctx, sample, backupDir, manifest)
	bucket, mapping := restoreBackupTargetB5RollbackObjects(t, ctx, sample, backupDir, manifest)
	verifyAndApplyBackupTargetB5RollbackObjectMap(t, ctx, sample, database, bucket, manifest, mapping)
	resetBackupTargetB5RollbackSyncDevices(t, ctx, sample, database, bucket, manifest)
	t.Logf("本轮专用容器 MySQL %s，MinIO %s，后端已关停", sample.fixture.source.mysqlContainer, sample.fixture.source.minioContainer)
}

func resetBackupTargetB5RollbackSyncDevices(t *testing.T, ctx context.Context, sample backupTargetB5RollbackSample,
	database backupTargetDatabase, bucket *serverUploadContentBucket, manifest backupSourceManifest,
) {
	t.Helper()
	// 1. 确认回退库是独占目标，导入的设备已经处于待重建状态。
	cutover, source := sample.fixture.database, sample.fixture.source
	if database.containerID == cutover.containerID || database.containerID == source.mysqlContainer ||
		database.name == cutover.name || database.name == source.databaseName ||
		bucket.name == sample.fixture.targetBucket.name || bucket.name != os.Getenv("CLOUDNEST_S3_BUCKET") {
		t.Fatal("离线换代未明确绑定到独立的回退库和回退桶")
	}
	userID := manifest.FileReferences[0].UserID
	before := readBackupB5DatabaseState(t, ctx, database.pool, userID)
	if !reflect.DeepEqual(before, sample.state) || len(before.devices) != 1 ||
		before.devices[0].UserID != userID || before.devices[0].State.Status != domain.SyncDeviceStatusResetRequired ||
		before.devices[0].State.AckSequence != 0 {
		t.Fatal("回退库应从切换后备份导入待重建且确认位置为零的原设备")
	}

	// 2. 运行预检和首次换代，从原清单与数据库复核代次和业务数据。
	address, instanceUUID, rosterDigest := verifyBackupB5OfflineSyncReset(t, ctx, database, bucket.name, manifest, before)

	// 3. 复查整套回退库范围及切换后库，确认换代仅作用于指定设备。
	verifyBackupTargetB5RollbackTableRows(t, ctx, database.pool)
	if after := readBackupB5DatabaseState(t, ctx, cutover.pool, userID); !reflect.DeepEqual(after, sample.state) {
		t.Fatal("回退库换代改变了切换后库的设备、日志或配额")
	}
	t.Logf("B5 回退库离线换代，地址 %s，UUID %s，待重建设备 %d 再换代，提交 1 条并复核 1 条，确认位置 0，日志 1～%d，原清单 SHA-256 %x",
		address, instanceUUID, before.devices[0].DeviceID, before.high, rosterDigest)
}
