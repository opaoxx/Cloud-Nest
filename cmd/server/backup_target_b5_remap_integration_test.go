package main

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestServerBackupTargetB5RemapIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后更新 B5 隔离目标库对象定位")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 用同一份备份准备目标库和目标桶。
	fixture := prepareBackupTargetB5Remap(t, ctx)
	verifyAndApplyBackupTargetB5ObjectMap(t, ctx, fixture)
	t.Logf("本轮专用容器 MySQL %s，MinIO %s，后端已关停", fixture.source.mysqlContainer, fixture.source.minioContainer)
}

func prepareBackupTargetB5Remap(t *testing.T, ctx context.Context) backupTargetRemapFixture {
	t.Helper()
	source := prepareBackupSource(t, ctx)
	backupDir := t.TempDir()
	manifest := prepareBackupSourceB5Manifest(t, ctx, source, backupDir)
	backupState := readBackupB5DatabaseState(t, ctx, source.pool, backupFixtureID(t, source.login.User.ID))
	return restoreBackupTargetB5Remap(t, ctx, source, backupDir, manifest, backupState)
}

func restoreBackupTargetB5Remap(t *testing.T, ctx context.Context, source backupSourceFixture,
	backupDir string, manifest backupSourceManifest, backupState backupB5DatabaseState,
) backupTargetRemapFixture {
	t.Helper()
	root := openBackupSourceBundleRoot(t, backupDir)
	saveAndVerifyBackupSourcePayloads(t, ctx, source.bucket, root, manifest.Objects)
	database := restoreBackupTargetB5Database(t, ctx, source, backupDir, manifest, backupState)
	targetBucket, mapping := restoreBackupTargetB5Objects(t, ctx, source, root, manifest)
	return backupTargetRemapFixture{source: source, database: database, targetBucket: targetBucket, manifest: manifest, mapping: mapping}
}

func verifyAndApplyBackupTargetB5ObjectMap(t *testing.T, ctx context.Context, fixture backupTargetRemapFixture) {
	t.Helper()
	if len(fixture.manifest.Objects) != 2 || len(fixture.manifest.HistoryReferences) != 1 {
		t.Fatal("隔离目标样本应包含一份当前及一份历史对象")
	}
	verifyBackupB5ObjectMapTransaction(t, ctx, fixture.database.pool, fixture.targetBucket.name, fixture.manifest, fixture.mapping)
}

func verifyBackupB5ObjectMapTransaction(t *testing.T, ctx context.Context, pool *sql.DB, bucket string,
	manifest backupSourceManifest, mapping backupTargetObjectMap,
) {
	t.Helper()
	// 1. 核对映射更新顺序和持久化基线，确保注入的错误发生在最后一行。
	count := len(manifest.Objects)
	if len(manifest.FileReferences) != 1 || len(manifest.HistoryReferences) == 0 ||
		count != 1+len(manifest.HistoryReferences) || len(mapping.Objects) != count ||
		manifest.Objects[0].ID != manifest.FileReferences[0].ObjectID {
		t.Fatal("事务夹具需要按当前对象、历史对象的顺序更新")
	}
	for index, location := range mapping.Objects {
		if location.ObjectID != manifest.Objects[index].ID ||
			(index > 0 && location.ObjectID != manifest.HistoryReferences[index-1].ObjectID) {
			t.Fatal("对象映射顺序与当前和历史引用不符", location.ObjectID)
		}
	}
	userID := manifest.FileReferences[0].UserID
	before := snapshotBackupTargetObjectsWithCount(t, ctx, pool, userID, count)
	stateBefore := readBackupB5DatabaseState(t, ctx, pool, userID)

	// 2. 让最后一条历史源版本失配，在数据库中读回整批回滚结果。
	last := count - 1
	historicalID := mapping.Objects[last].ObjectID
	staleManifest, staleMapping := manifest, mapping
	staleManifest.Objects = slices.Clone(manifest.Objects)
	staleMapping.Objects = slices.Clone(mapping.Objects)
	staleVersion := "stale-source-version"
	staleManifest.Objects[last].VersionID = &staleVersion
	staleMapping.Objects[last].SourceVersionID = &staleVersion
	updated, err := applyBackupTargetObjectMap(ctx, pool, bucket, staleManifest, staleMapping)
	if updated != 0 || err == nil || !strings.Contains(err.Error(), fmt.Sprintf("对象 %d 的预期源定位未匹配", historicalID)) {
		t.Fatalf("历史对象源版本失配时应回滚整批，已提交 %d 行，错误 %v", updated, err)
	}
	if afterFailure := snapshotBackupTargetObjectsWithCount(t, ctx, pool, userID, count); !reflect.DeepEqual(afterFailure, before) {
		t.Fatal("事务失败后目标对象仍有部分更新")
	}
	t.Logf("B5 对象定位回滚，第 %d 条历史对象 %d 的源版本失配，全部 %d 条保持原定位", count, historicalID, count)

	// 3. 提交真实映射，核对对象字段、业务引用和配额。
	updated, err = applyBackupTargetObjectMap(ctx, pool, bucket, manifest, mapping)
	if err != nil || updated != count {
		t.Fatalf("目标库应一次提交 %d 条对象定位，已提交 %d 行，错误 %v", count, updated, err)
	}
	expectedRows := maps.Clone(before)
	expectedManifest := manifest
	expectedManifest.Objects = slices.Clone(manifest.Objects)
	for index, location := range mapping.Objects {
		expected := expectedRows[location.ObjectID]
		expected.Bucket, expected.StorageVersionID, expected.StorageETag = location.TargetBucket, &location.TargetVersionID, &location.TargetETag
		expectedRows[location.ObjectID] = expected
		expectedManifest.Objects[index].Bucket = location.TargetBucket
		expectedManifest.Objects[index].VersionID = &location.TargetVersionID
	}
	if after := snapshotBackupTargetObjectsWithCount(t, ctx, pool, userID, count); !reflect.DeepEqual(after, expectedRows) {
		t.Fatal("目标对象定位与版本映射不符，或其他对象字段发生变化")
	}
	actualManifest, err := readBackupSourceManifest(ctx, pool, bucket, manifest.DatabaseSHA256)
	if err != nil || !reflect.DeepEqual(actualManifest, expectedManifest) {
		t.Fatal("目标库的当前引用、历史引用或物理对象字段不符", err)
	}
	if stateAfter := readBackupB5DatabaseState(t, ctx, pool, userID); !reflect.DeepEqual(stateAfter, stateBefore) {
		t.Fatal("目标库配额或同步状态发生变化")
	}
	t.Logf("B5 目标提交 %d 条对象定位，当前和历史引用一致，已用 %d B、预占 %d B，映射绑定清单 SHA-256 %s",
		count, stateBefore.used, stateBefore.reserved, mapping.ManifestSHA256)
}
