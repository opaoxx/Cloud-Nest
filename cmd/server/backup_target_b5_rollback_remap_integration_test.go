package main

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestServerBackupTargetB5RollbackRemapIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后验证 B5 回退库对象定位")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 从同一轮备份重建停服回退库和对象映射。
	sample := prepareBackupTargetB5RollbackSample(t, ctx)
	backupDir := t.TempDir()
	manifest := exportBackupTargetB5RollbackBundle(t, ctx, sample, backupDir)
	database := restoreBackupTargetB5RollbackDatabase(t, ctx, sample, backupDir, manifest)
	bucket, mapping := restoreBackupTargetB5RollbackObjects(t, ctx, sample, backupDir, manifest)
	verifyAndApplyBackupTargetB5RollbackObjectMap(t, ctx, sample, database, bucket, manifest, mapping)
	t.Logf("本轮专用容器 MySQL %s，MinIO %s，后端已关停", sample.fixture.source.mysqlContainer, sample.fixture.source.minioContainer)
}

func verifyAndApplyBackupTargetB5RollbackObjectMap(t *testing.T, ctx context.Context, sample backupTargetB5RollbackSample,
	database backupTargetDatabase, bucket *serverUploadContentBucket, manifest backupSourceManifest, mapping backupTargetObjectMap,
) {
	t.Helper()
	// 1. 复核回退库和回退桶的作用范围，记录尚未更新的对象定位。
	cutover := sample.fixture.database
	var databaseName string
	if err := database.pool.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&databaseName); err != nil ||
		databaseName != database.name || databaseName == cutover.name || databaseName == sample.fixture.source.databaseName ||
		database.containerID == cutover.containerID || database.containerID == sample.fixture.source.mysqlContainer ||
		bucket.name != os.Getenv("CLOUDNEST_S3_BUCKET") || bucket.name == sample.fixture.targetBucket.name ||
		len(manifest.FileReferences) != 1 || len(manifest.HistoryReferences) != 2 || len(mapping.Objects) != 3 {
		t.Fatal("对象定位更新未明确绑定回退库、回退桶和三条已核对的映射", err)
	}
	userID := manifest.FileReferences[0].UserID
	cutoverObjects := snapshotBackupTargetObjectsWithCount(t, ctx, cutover.pool, userID, 3)
	importedObjects := snapshotBackupTargetObjectsWithCount(t, ctx, database.pool, userID, 3)
	if !reflect.DeepEqual(importedObjects, cutoverObjects) {
		t.Fatal("回退库在更新前未保留完整的切换后对象记录")
	}

	// 2. 让最后一条源定位失配，核对三条记录全部保留原值。
	// 3. 提交真实映射，核对新定位及原有引用、日志和配额。
	verifyBackupB5ObjectMapTransaction(t, ctx, database.pool, bucket.name, manifest, mapping)
	if after := readBackupB5DatabaseState(t, ctx, database.pool, userID); !reflect.DeepEqual(after, sample.state) {
		t.Fatal("回退库的停写配额、五条日志或设备记录发生变化")
	}
	if after := snapshotBackupTargetObjectsWithCount(t, ctx, cutover.pool, userID, 3); !reflect.DeepEqual(after, cutoverObjects) {
		t.Fatal("回退库映射作业改变了切换后样本库的对象记录")
	}
	verifyBackupTargetVersions(t, ctx, bucket, mapping.Objects)
	t.Logf("B5 回退库对象定位，当前 v3 与历史 v1/v2 共提交 3 条，保留日志 1～%d，回退桶 %s，映射绑定清单 SHA-256 %s",
		sample.state.high, bucket.name, mapping.ManifestSHA256)
}
