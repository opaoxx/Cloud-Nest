package main

import (
	"context"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestServerBackupTargetB5ObjectsIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后恢复隔离 B5 目标桶")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 生成同一次停写的备份，并从磁盘核对两个对象正文。
	source := prepareBackupSource(t, ctx)
	backupDir := t.TempDir()
	manifest := prepareBackupSourceB5Manifest(t, ctx, source, backupDir)
	root := openBackupSourceBundleRoot(t, backupDir)
	saveAndVerifyBackupSourcePayloads(t, ctx, source.bucket, root, manifest.Objects)
	restoreBackupTargetB5Objects(t, ctx, source, root, manifest)
	t.Logf("本轮专用容器 MySQL %s，MinIO %s，后端已关停", source.mysqlContainer, source.minioContainer)
}

func restoreBackupTargetB5Objects(t *testing.T, ctx context.Context, source backupSourceFixture, root *os.Root,
	manifest backupSourceManifest,
) (*serverUploadContentBucket, backupTargetObjectMap) {
	t.Helper()
	target, writer := prepareBackupTargetBucket(t, ctx, source)

	// 2. 摘要错误时保持空桶，再按原对象键恢复并读回两个新版本。
	historicalID := manifest.HistoryReferences[0].ObjectID
	historicalIndex := slices.IndexFunc(manifest.Objects, func(object backupManifestObject) bool {
		return object.ID == historicalID
	})
	if historicalIndex < 0 {
		t.Fatal("清单没有历史引用对应的物理对象")
	}
	historicalObject := manifest.Objects[historicalIndex]
	wrongDigest := historicalObject
	wrongDigest.SHA256 = strings.Repeat("0", 64)
	if _, err := restoreBackupObject(ctx, writer, target, root, wrongDigest); err == nil ||
		!strings.Contains(err.Error(), "本机备份正文") {
		t.Fatalf("历史正文摘要错误时应拒绝写入 %v", err)
	}
	verifyBackupTargetVersions(t, ctx, target, nil)
	objects := restoreAndVerifyBackupTargetObjects(t, ctx, writer, target, root, manifest.Objects)
	if _, err := restoreBackupObject(ctx, writer, target, root, historicalObject); !errors.Is(err, domain.ErrUploadObjectExists) {
		t.Fatalf("重复恢复历史对象应被条件写入拒绝 %v", err)
	}

	// 3. 从磁盘读回映射，按清单比较源定位，再核对目标桶实际版本。
	writeAndVerifyBackupTargetObjectMap(t, root, objects)
	loadedManifest, mapping, err := readBackupRemapFiles(root)
	if err != nil || !reflect.DeepEqual(loadedManifest, manifest) || len(mapping.Objects) != len(manifest.Objects) {
		t.Fatal("B5 版本映射没有绑定同一份备份清单", err)
	}
	for index, location := range mapping.Objects {
		original := manifest.Objects[index]
		if location.ObjectID != original.ID || location.UserID != original.UserID || location.Provider != original.Provider ||
			location.ObjectKey != original.Key || location.SourceBucket != original.Bucket ||
			!reflect.DeepEqual(location.SourceVersionID, original.VersionID) || location.TargetBucket != target.name {
			t.Fatalf("对象 %d 的版本映射未保留原始归属或物理定位", original.ID)
		}
	}
	verifyBackupTargetVersions(t, ctx, target, mapping.Objects)
	t.Logf("B5 目标 MinIO 只含两个已核验的版本，历史对象 %d，当前对象 %d，版本映射绑定清单 SHA-256 %s",
		historicalID, manifest.FileReferences[0].ObjectID, mapping.ManifestSHA256)
	return target, mapping
}
