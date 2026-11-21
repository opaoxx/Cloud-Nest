package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
)

type backupTargetRemapFixture struct {
	source       backupSourceFixture
	database     backupTargetDatabase
	targetBucket *serverUploadContentBucket
	manifest     backupSourceManifest
	mapping      backupTargetObjectMap
}

func TestServerBackupTargetRemapIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后更新隔离目标库对象定位")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 在同一轮生成源端备份、导入隔离目标库，恢复并核对目标对象版本。
	fixture := prepareBackupTargetRemap(t, ctx)
	database, targetBucket, loadedManifest, mapping := fixture.database, fixture.targetBucket, fixture.manifest, fixture.mapping

	// 2. 模拟第二条源版本不匹配。前一条即使更新成功，事务结束后也应回滚。
	userID := backupFixtureID(t, fixture.source.login.User.ID)
	before := snapshotBackupTargetObjects(t, ctx, database.pool, userID)
	staleManifest, staleMapping := loadedManifest, mapping
	staleManifest.Objects = slices.Clone(loadedManifest.Objects)
	staleMapping.Objects = slices.Clone(mapping.Objects)
	staleVersion := "stale-source-version"
	staleManifest.Objects[1].VersionID = &staleVersion
	staleMapping.Objects[1].SourceVersionID = &staleVersion
	updated, err := applyBackupTargetObjectMap(ctx, database.pool, targetBucket.name, staleManifest, staleMapping)
	wantError := fmt.Sprintf("对象 %d 的预期源定位未匹配", staleMapping.Objects[1].ObjectID)
	if err == nil || !strings.Contains(err.Error(), wantError) || updated != 0 {
		t.Fatalf("第二条条件失败没有终止事务：已提交 %d 行，错误 %v", updated, err)
	}
	if afterFailure := snapshotBackupTargetObjects(t, ctx, database.pool, userID); !reflect.DeepEqual(afterFailure, before) {
		t.Fatal("事务失败后仍存在被部分更新的对象")
	}
	t.Log("第二条源版本复核仅匹配零行，事务回滚；第一条已执行的更新未保留")

	// 3. 提交实际映射，核对三条定位、全部对象字段、文件引用和配额。
	updated, err = applyBackupTargetObjectMap(ctx, database.pool, targetBucket.name, loadedManifest, mapping)
	if err != nil || updated != 3 {
		t.Fatalf("目标库应提交三个对象的定位：已提交 %d 行，错误 %v", updated, err)
	}
	expectedManifest := loadedManifest
	expectedManifest.Objects = slices.Clone(loadedManifest.Objects)
	for _, location := range mapping.Objects {
		object := before[location.ObjectID]
		object.Bucket, object.StorageVersionID, object.StorageETag = location.TargetBucket, &location.TargetVersionID, &location.TargetETag
		before[location.ObjectID] = object
		for index := range expectedManifest.Objects {
			if expectedManifest.Objects[index].ID == location.ObjectID {
				expectedManifest.Objects[index].Bucket = location.TargetBucket
				expectedManifest.Objects[index].VersionID = &location.TargetVersionID
				break
			}
		}
	}
	after := snapshotBackupTargetObjects(t, ctx, database.pool, userID)
	if !reflect.DeepEqual(after, before) {
		t.Fatal("提交后的对象字段与目标映射不符，或暂存对象发生变化")
	}
	actualManifest, err := readBackupSourceManifest(ctx, database.pool, targetBucket.name, loadedManifest.DatabaseSHA256)
	if err != nil || !reflect.DeepEqual(actualManifest, expectedManifest) {
		t.Fatal("更新后目录、文件引用或对象摘要与目标基线不符", err)
	}
	inspectBackupDatabase(t, ctx, database.pool, userID)
	t.Log("目标库提交三条对象定位，目录和文件引用保持一致，无正文的暂存对象原样，已用 10 B、预占 3 B")
}

func prepareBackupTargetRemap(t *testing.T, ctx context.Context) backupTargetRemapFixture {
	t.Helper()
	source := prepareBackupSource(t, ctx)
	root, manifest := prepareBackupSourceBundle(t, ctx, source)
	saveAndVerifyBackupSourcePayloads(t, ctx, source.bucket, root, manifest.Objects)
	t.Logf("备份包就绪，开始恢复到隔离目标：UTC %s", time.Now().UTC().Format(time.RFC3339Nano))
	database := prepareBackupTargetDatabase(t, ctx, source)
	if err := importBackupTargetSQL(ctx, database.pool, database.containerID, database.name,
		filepath.Join(root.Name(), "database.sql"), manifest.DatabaseSHA256); err != nil {
		t.Fatal(err)
	}
	targetBucket, writer := prepareBackupTargetBucket(t, ctx, source)
	objects := restoreAndVerifyBackupTargetObjects(t, ctx, writer, targetBucket, root, manifest.Objects)
	verifyBackupTargetVersions(t, ctx, targetBucket, objects)
	writeAndVerifyBackupTargetObjectMap(t, root, objects)
	loadedManifest, mapping, err := readBackupRemapFiles(root)
	if err != nil || !reflect.DeepEqual(loadedManifest, manifest) {
		t.Fatal("读回复制映射或备份清单失败", err)
	}
	return backupTargetRemapFixture{source: source, database: database, targetBucket: targetBucket, manifest: loadedManifest, mapping: mapping}
}

func readBackupRemapFiles(root *os.Root) (backupSourceManifest, backupTargetObjectMap, error) {
	manifestBytes, err := root.ReadFile("manifest.json")
	if err != nil {
		return backupSourceManifest{}, backupTargetObjectMap{}, err
	}
	mappingBytes, err := root.ReadFile("object-version-map.json")
	if err != nil {
		return backupSourceManifest{}, backupTargetObjectMap{}, err
	}
	var manifest backupSourceManifest
	var mapping backupTargetObjectMap
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return backupSourceManifest{}, backupTargetObjectMap{}, err
	}
	if err := json.Unmarshal(mappingBytes, &mapping); err != nil {
		return backupSourceManifest{}, backupTargetObjectMap{}, err
	}
	if mapping.ManifestSHA256 != fmt.Sprintf("%x", sha256.Sum256(manifestBytes)) {
		return backupSourceManifest{}, backupTargetObjectMap{}, errors.New("对象版本映射与备份清单不匹配")
	}
	return manifest, mapping, nil
}

func applyBackupTargetObjectMap(ctx context.Context, pool *sql.DB, targetBucket string, manifest backupSourceManifest, mapping backupTargetObjectMap) (int, error) {
	// 1. 核对所有映射的对象归属、预期源定位和目标桶后再开始事务。
	if targetBucket == "" || len(manifest.Objects) == 0 || len(manifest.Objects) != len(mapping.Objects) {
		return 0, errors.New("对象版本映射数量或目标桶不符")
	}
	byID := make(map[uint64]backupManifestObject, len(manifest.Objects))
	for _, object := range manifest.Objects {
		byID[object.ID] = object
	}
	seen := make(map[uint64]bool, len(mapping.Objects))
	for _, location := range mapping.Objects {
		object, ok := byID[location.ObjectID]
		if !ok || seen[location.ObjectID] || location.UserID != object.UserID || location.Provider != "local_minio" ||
			location.Provider != object.Provider || location.SourceBucket != object.Bucket || location.ObjectKey != object.Key ||
			!reflect.DeepEqual(location.SourceVersionID, object.VersionID) || location.TargetBucket != targetBucket ||
			location.SourceBucket == targetBucket || location.TargetVersionID == "" || location.TargetVersionID == "null" || location.TargetETag == "" {
			return 0, fmt.Errorf("对象 %d 的版本映射与清单或目标桶不符", location.ObjectID)
		}
		seen[location.ObjectID] = true
	}
	transaction, err := pool.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer transaction.Rollback()

	// 2. 只更新三个存储定位字段，要求逐行匹配源标识和已经核验的大小与摘要。
	for _, location := range mapping.Objects {
		object := byID[location.ObjectID]
		result, err := transaction.ExecContext(ctx, `UPDATE stored_objects
			SET bucket=?,storage_version_id=?,storage_etag=?
			WHERE id=? AND owner_user_id=? AND status='READY' AND provider=? AND bucket=? AND object_key=?
			AND storage_version_id <=> ? AND file_size=? AND verified_sha256=UNHEX(?)`,
			location.TargetBucket, location.TargetVersionID, location.TargetETag, location.ObjectID, location.UserID,
			location.Provider, location.SourceBucket, location.ObjectKey, location.SourceVersionID, object.SizeBytes, object.SHA256)
		if err != nil {
			return 0, fmt.Errorf("更新对象 %d 的版本定位失败: %w", location.ObjectID, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		if affected != 1 {
			return 0, fmt.Errorf("对象 %d 的预期源定位未匹配，更新 %d 行", location.ObjectID, affected)
		}
	}

	// 3. 全部条件满足后提交，任一行失败则由延迟回滚撤销整批。
	if err := transaction.Commit(); err != nil {
		return 0, err
	}
	return len(mapping.Objects), nil
}

func snapshotBackupTargetObjects(t *testing.T, ctx context.Context, pool *sql.DB, userID uint64) map[uint64]domain.StoredObject {
	t.Helper()
	rows, err := pool.QueryContext(ctx, "SELECT id FROM stored_objects WHERE owner_user_id=?", userID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []uint64
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil || len(ids) != 4 {
		t.Fatal("目标库应包含三条已发布记录和一条暂存记录", err)
	}
	store, err := repository.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	objects := make(map[uint64]domain.StoredObject, len(ids))
	for _, id := range ids {
		object, err := store.FindUserStoredObject(ctx, userID, id)
		if err != nil || object == nil {
			t.Fatal("读取目标对象失败", id, err)
		}
		objects[id] = *object
	}
	return objects
}
