package main

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestServerBackupTargetB5RollbackObjectsIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后验证 B5 回退桶对象恢复")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	sample := prepareBackupTargetB5RollbackSample(t, ctx)
	backupDir := t.TempDir()
	manifest := exportBackupTargetB5RollbackBundle(t, ctx, sample, backupDir)
	database := restoreBackupTargetB5RollbackDatabase(t, ctx, sample, backupDir, manifest)
	bucket, mapping := restoreBackupTargetB5RollbackObjects(t, ctx, sample, backupDir, manifest)

	// 4. 回退库仍保留备份中的对象定位及原账号状态。
	persisted, err := readBackupSourceManifest(ctx, database.pool, sample.fixture.targetBucket.name, manifest.DatabaseSHA256)
	if err != nil || !reflect.DeepEqual(persisted, manifest) {
		t.Fatal("恢复正文后回退库的引用或原对象定位发生变化", err)
	}
	userID := manifest.FileReferences[0].UserID
	if state := readBackupB5DatabaseState(t, ctx, database.pool, userID); !reflect.DeepEqual(state, sample.state) {
		t.Fatal("恢复正文后回退库的配额、日志或设备状态发生变化")
	}
	t.Logf("B5 回退桶正文恢复，桶 %s，当前 v3 与历史 v1/v2 共 %d 个新版本，合计 %d B，映射绑定清单 SHA-256 %s",
		bucket.name, len(mapping.Objects), sample.state.used, mapping.ManifestSHA256)
	t.Logf("本轮专用容器 MySQL %s，MinIO %s，后端已关停", sample.fixture.source.mysqlContainer, sample.fixture.source.minioContainer)
}

func restoreBackupTargetB5RollbackObjects(t *testing.T, ctx context.Context, sample backupTargetB5RollbackSample,
	backupDir string, manifest backupSourceManifest,
) (*serverUploadContentBucket, backupTargetObjectMap) {
	t.Helper()
	// 1. 读取同一轮备份，准备独立的空版本化桶。
	cutover := sample.fixture.targetBucket
	if os.Getenv("CLOUDNEST_S3_BUCKET") != cutover.name || len(manifest.FileReferences) != 1 ||
		len(manifest.HistoryReferences) != 2 || len(manifest.Objects) != 3 {
		t.Fatal("回退对象恢复需要同一切换后备份的当前版本及两份历史")
	}
	for _, object := range manifest.Objects {
		if object.Bucket != cutover.name || object.VersionID == nil {
			t.Fatal("备份缺少切换后桶中对象的实际物理版本", object.ID)
		}
	}
	root, err := os.OpenRoot(backupDir)
	if err != nil {
		t.Fatal("打开本轮回退备份失败", err)
	}
	defer root.Close()
	verifySavedBackupPayloads(t, root, manifest.Objects)
	target, writer := prepareBackupTargetBucket(t, ctx, sample.fixture.source)
	if target.name == cutover.name {
		t.Fatal("回退桶必须独立于切换后样本桶")
	}

	// 2. 验证摘要失配会被拒绝，恢复并读回三个版本。
	current := manifest.Objects[0]
	if current.ID != manifest.FileReferences[0].ObjectID {
		t.Fatal("备份清单的首个对象应为切换后新发布的当前版本")
	}
	wrongDigest := current
	wrongDigest.SHA256 = strings.Repeat("0", 64)
	if _, err := restoreBackupObject(ctx, writer, target, root, wrongDigest); err == nil ||
		!strings.Contains(err.Error(), "本机备份正文") {
		t.Fatal("新版本的备份摘要失配应在写入前被拒绝", err)
	}
	verifyBackupTargetVersions(t, ctx, target, nil)
	objects := restoreAndVerifyBackupTargetObjects(t, ctx, writer, target, root, manifest.Objects)
	if _, err := restoreBackupObject(ctx, writer, target, root, current); !errors.Is(err, domain.ErrUploadObjectExists) {
		t.Fatal("重复恢复当前对象应被条件写入拒绝", err)
	}

	// 3. 保存并读回版本映射，核对回退桶和切换后桶中的物理版本。
	writeAndVerifyBackupTargetObjectMap(t, root, objects)
	loadedManifest, mapping, err := readBackupRemapFiles(root)
	if err != nil || !reflect.DeepEqual(loadedManifest, manifest) || len(mapping.Objects) != len(manifest.Objects) {
		t.Fatal("回退对象映射没有绑定原备份清单", err)
	}
	for index, location := range mapping.Objects {
		original := manifest.Objects[index]
		if location.ObjectID != original.ID || location.UserID != original.UserID || location.Provider != original.Provider ||
			location.ObjectKey != original.Key || location.SourceBucket != cutover.name ||
			!reflect.DeepEqual(location.SourceVersionID, original.VersionID) || location.TargetBucket != target.name ||
			location.TargetVersionID == *original.VersionID || location.TargetETag == "" {
			t.Fatal("回退对象映射中的源定位或目标版本不符", original.ID)
		}
	}
	verifyBackupTargetVersions(t, ctx, target, mapping.Objects)
	verifyBackupManifestObjectVersions(t, ctx, cutover, manifest.Objects)
	return target, mapping
}
