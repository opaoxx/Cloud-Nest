package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestServerBackupSourceB5ManifestIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后生成 B5 隔离源端清单")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 准备同一停写点的源库和 SQL。
	source := prepareBackupSource(t, ctx)
	verifyBackupSourceSyncBaseline(t, ctx, source)
	fixture := prepareReplacementPublicationFixtureFromSource(t, ctx, source)
	verifyBackupSourceFileVersions(t, ctx, fixture)
	backupDir := t.TempDir()
	dump := stopAndExportBackupSourceB5SQL(t, ctx, fixture, backupDir)
	writeAndVerifyBackupSourceB5Manifest(t, ctx, fixture, backupDir, dump)
	t.Logf("本轮专用容器 MySQL %s，MinIO %s，后端已关停", source.mysqlContainer, source.minioContainer)
}

func writeAndVerifyBackupSourceB5Manifest(t *testing.T, ctx context.Context, fixture replacementPublicationFixture,
	backupDir string, dump []byte,
) backupSourceManifest {
	t.Helper()

	// 2. 生成清单并读回，确认实际导出文件的摘要与清单一致。
	manifest, saved := writeBackupSourceManifest(t, ctx, fixture.backupSourceFixture, backupDir, dump)
	sqlOnDisk, err := os.ReadFile(filepath.Join(backupDir, "database.sql"))
	if err != nil || manifest.DatabaseSHA256 != fmt.Sprintf("%x", sha256.Sum256(sqlOnDisk)) {
		t.Fatal("清单没有绑定同一停写窗口的 SQL 文件", err)
	}
	if len(manifest.Directories) != 3 || len(manifest.FileReferences) != 1 ||
		len(manifest.HistoryReferences) != 1 || len(manifest.Objects) != 2 {
		t.Fatalf("B5 清单范围不符，目录 %d，当前 %d，历史 %d，物理对象 %d", len(manifest.Directories),
			len(manifest.FileReferences), len(manifest.HistoryReferences), len(manifest.Objects))
	}
	current, history := manifest.FileReferences[0], manifest.HistoryReferences[0]
	currentContent, err := fixture.store.FindUserFileEntryContent(ctx, fixture.userID, fixture.target.ID)
	if err != nil || currentContent == nil || currentContent.ObjectID == nil || current.ID != fixture.target.ID ||
		current.UserID != fixture.userID || current.Revision != fixture.target.Revision+1 || current.ObjectID != *currentContent.ObjectID {
		t.Fatal("清单的当前引用未匹配停写库的当前文件", err)
	}
	var historyID uint64
	var createdAt, retainedUntil time.Time
	err = fixture.pool.QueryRowContext(ctx, `SELECT id,created_at,retained_until FROM file_versions
		WHERE user_id=? AND entry_id=? AND revision=?`, fixture.userID, fixture.target.ID, fixture.target.Revision).
		Scan(&historyID, &createdAt, &retainedUntil)
	if err != nil || history.ID != historyID || history.EntryID != current.ID || history.UserID != current.UserID ||
		history.Revision != fixture.target.Revision || history.ObjectID != fixture.originalObject.ID ||
		history.ObjectID == current.ObjectID || history.SizeBytes != uint64(len(fixture.original)) ||
		!history.CreatedAt.Equal(createdAt) || !history.RetainedUntil.Equal(retainedUntil) {
		t.Fatal("清单的历史引用未匹配停写库中的旧版本和保留时间", err)
	}

	// 3. 两类引用各自指向一个已核对的物理版本，正文路径按对象 ID 唯一生成。
	expectedSizes := map[uint64]uint64{current.ObjectID: current.SizeBytes, history.ObjectID: history.SizeBytes}
	for _, object := range manifest.Objects {
		expectedSize, referenced := expectedSizes[object.ID]
		stored, err := fixture.store.FindUserStoredObject(ctx, fixture.userID, object.ID)
		if !referenced || err != nil || stored == nil || object.UserID != fixture.userID ||
			object.Bucket != stored.Bucket || object.Key != stored.ObjectKey || object.VersionID == nil ||
			stored.StorageVersionID == nil || *object.VersionID != *stored.StorageVersionID || object.SizeBytes != expectedSize ||
			object.SizeBytes != stored.FileSize || stored.VerifiedSHA256 == nil || object.SHA256 != fmt.Sprintf("%x", *stored.VerifiedSHA256) {
			t.Fatalf("对象 %d 未一一对应停写库的归属、引用、位置和摘要，错误 %v", object.ID, err)
		}
		delete(expectedSizes, object.ID)
	}
	if len(expectedSizes) != 0 {
		t.Fatal("存在未登记物理版本的当前或历史引用")
	}
	verifyBackupManifestObjectVersions(t, ctx, fixture.bucket, manifest.Objects)
	t.Logf("B5 清单 %d B，SHA-256 %x，绑定 SQL SHA-256 %s，当前 v%d→对象 %d，历史 v%d→对象 %d，桶内 2 个版本一致",
		len(saved), sha256.Sum256(saved), manifest.DatabaseSHA256, current.Revision, current.ObjectID, history.Revision, history.ObjectID)
	return manifest
}
