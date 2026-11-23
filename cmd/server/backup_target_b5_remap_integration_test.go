package main

import (
	"context"
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
	root := openBackupSourceBundleRoot(t, backupDir)
	saveAndVerifyBackupSourcePayloads(t, ctx, source.bucket, root, manifest.Objects)
	database := restoreBackupTargetB5Database(t, ctx, source, backupDir, manifest)
	targetBucket, mapping := restoreBackupTargetB5Objects(t, ctx, source, root, manifest)
	return backupTargetRemapFixture{source: source, database: database, targetBucket: targetBucket, manifest: manifest, mapping: mapping}
}

func verifyAndApplyBackupTargetB5ObjectMap(t *testing.T, ctx context.Context, fixture backupTargetRemapFixture) {
	t.Helper()
	pool, bucket, manifest, mapping := fixture.database.pool, fixture.targetBucket.name, fixture.manifest, fixture.mapping
	userID := backupFixtureID(t, fixture.source.login.User.ID)
	before := snapshotBackupTargetObjectsWithCount(t, ctx, pool, userID, 2)
	stateBefore := readBackupB5DatabaseState(t, ctx, pool, userID)

	// 2. 复验第二条源版本失配时整批回滚。
	historicalID := manifest.HistoryReferences[0].ObjectID
	if len(mapping.Objects) != 2 || mapping.Objects[0].ObjectID != manifest.FileReferences[0].ObjectID ||
		mapping.Objects[1].ObjectID != historicalID || manifest.Objects[1].ID != historicalID {
		t.Fatal("回滚夹具需要先更新当前对象，再命中历史对象的失败条件")
	}
	staleManifest, staleMapping := manifest, mapping
	staleManifest.Objects = slices.Clone(manifest.Objects)
	staleMapping.Objects = slices.Clone(mapping.Objects)
	staleVersion := "stale-source-version"
	staleManifest.Objects[1].VersionID = &staleVersion
	staleMapping.Objects[1].SourceVersionID = &staleVersion
	updated, err := applyBackupTargetObjectMap(ctx, pool, bucket, staleManifest, staleMapping)
	if updated != 0 || err == nil || !strings.Contains(err.Error(), fmt.Sprintf("对象 %d 的预期源定位未匹配", historicalID)) {
		t.Fatalf("历史对象源版本失配时应回滚整批，已提交 %d 行，错误 %v", updated, err)
	}
	if afterFailure := snapshotBackupTargetObjectsWithCount(t, ctx, pool, userID, 2); !reflect.DeepEqual(afterFailure, before) {
		t.Fatal("事务失败后目标对象仍有部分更新")
	}
	t.Log("B5 目标历史对象源版本不符，整批回滚，当前对象仍保留原定位")

	// 3. 提交真实映射，核对对象字段、业务引用和配额。
	updated, err = applyBackupTargetObjectMap(ctx, pool, bucket, manifest, mapping)
	if err != nil || updated != 2 {
		t.Fatalf("目标库应一次提交两条对象定位，已提交 %d 行，错误 %v", updated, err)
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
	if after := snapshotBackupTargetObjectsWithCount(t, ctx, pool, userID, 2); !reflect.DeepEqual(after, expectedRows) {
		t.Fatal("目标对象定位与版本映射不符，或其他对象字段发生变化")
	}
	actualManifest, err := readBackupSourceManifest(ctx, pool, bucket, manifest.DatabaseSHA256)
	if err != nil || !reflect.DeepEqual(actualManifest, expectedManifest) {
		t.Fatal("目标库的当前引用、历史引用或物理对象字段不符", err)
	}
	if stateAfter := readBackupB5DatabaseState(t, ctx, pool, userID); !reflect.DeepEqual(stateAfter, stateBefore) ||
		stateAfter.used != 72 || stateAfter.reserved != 0 {
		t.Fatal("目标库配额或同步状态发生变化")
	}
	t.Logf("B5 目标提交 2 条对象定位，当前和历史引用一致，已用 72 B、预占 0 B，映射绑定清单 SHA-256 %s", mapping.ManifestSHA256)
}
