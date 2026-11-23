package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

func TestServerBackupSourceFileVersionsIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后核对隔离源端的当前和历史正文")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 发布原文件，记录原条目和对象。
	fixture := prepareReplacementPublicationFixture(t, ctx)
	verifyBackupSourceFileVersions(t, ctx, fixture)
	logs, err := fixture.stop()
	if err != nil {
		t.Fatal("隔离源端关停失败", err)
	}
	verifyBackupSourceStopped(t, fixture.api.baseURL, logs)
	t.Logf("本轮专用容器 MySQL %s，MinIO %s，后端已关停", fixture.mysqlContainer, fixture.minioContainer)
}

func verifyBackupSourceFileVersions(t *testing.T, ctx context.Context, fixture replacementPublicationFixture) {
	t.Helper()

	// 2. 替换同一文件的内容，等待后台发布。
	replacement := []byte("cloudnest backup current content\n")
	digest := sha256.Sum256(replacement)
	body := fmt.Sprintf(`{"expected_revision":"%d","size_bytes":%d,"sha256":"%x"}`, fixture.target.Revision, len(replacement), digest)
	var created dto.CreateUploadResponse
	fixture.api.request(t, ctx, http.MethodPost, fixture.entryPath+"/content-replacement", "backup-source-current-version",
		strings.NewReader(body), http.StatusCreated, &created)
	taskID := backupFixtureID(t, created.Data.Task.ID)
	if created.Data.Task.Status != "UPLOADING" || created.Data.Task.SizeBytes != uint64(len(replacement)) ||
		created.Data.ContentPath != "/api/v1/uploads/"+created.Data.Task.ID+"/content" {
		t.Fatal("未交付与合成样本匹配的 SINGLE 替换任务")
	}
	sendSingleReplacementContent(t, ctx, fixture.api, created.Data, replacement)
	waitContext, stopWait := context.WithTimeout(ctx, 25*time.Second)
	defer stopWait()
	published := awaitServerUploadPublication(t, waitContext, fixture.store, fixture.userID, taskID)
	entryID := fixture.target.ID
	currentRevision := fixture.target.Revision + 1
	if published.ResultEntryID == nil || *published.ResultEntryID != entryID || published.PublishedObjectID == nil ||
		*published.PublishedObjectID == fixture.originalObject.ID || published.ReplacementPublishedRevision == nil ||
		*published.ReplacementPublishedRevision != currentRevision || published.ReservedBytes != 0 {
		t.Fatal("替换未在同一文件提交新内容版本")
	}

	// 3. 按数据库记录的确切版本读回两份正文，复核配额。
	current, err := fixture.store.FindUserFileEntry(ctx, fixture.userID, entryID)
	if err != nil || current == nil || current.Status != "ACTIVE" || current.Revision != currentRevision ||
		current.FileSize != uint64(len(replacement)) {
		t.Fatal("当前文件记录与发布结果不符", err)
	}
	content, err := fixture.store.FindUserFileEntryContent(ctx, fixture.userID, entryID)
	if err != nil || content == nil || content.ObjectID == nil || *content.ObjectID != *published.PublishedObjectID {
		t.Fatal("当前文件未指向本次发布对象", err)
	}
	var history domain.FileVersion
	err = fixture.pool.QueryRowContext(ctx, `SELECT id,user_id,entry_id,revision,object_id,file_size,created_at,retained_until
		FROM file_versions WHERE user_id=? AND entry_id=?`, fixture.userID, entryID).
		Scan(&history.ID, &history.UserID, &history.EntryID, &history.Revision, &history.ObjectID, &history.FileSize, &history.CreatedAt, &history.RetainedUntil)
	if err != nil || !history.IsValid() || history.Revision != fixture.target.Revision || history.ObjectID != fixture.originalObject.ID ||
		history.FileSize != uint64(len(fixture.original)) || !history.RetainedUntil.After(time.Now().UTC()) {
		t.Fatal("历史行未保留未到期的原内容引用", err)
	}
	var historyCount int
	if err := fixture.pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM file_versions WHERE user_id=? AND entry_id=?",
		fixture.userID, entryID).Scan(&historyCount); err != nil || historyCount != 1 {
		t.Fatalf("期望同一文件只保留一份历史，实际 %d，错误 %v", historyCount, err)
	}
	historicalObject := verifyBackupSourceObjectVersion(t, ctx, fixture, "历史", history.ObjectID, history.Revision, fixture.original)
	currentObject := verifyBackupSourceObjectVersion(t, ctx, fixture, "当前", *content.ObjectID, currentRevision, replacement)
	if historicalObject.ObjectKey != fixture.originalObject.ObjectKey || *historicalObject.StorageVersionID != *fixture.originalObject.StorageVersionID ||
		historicalObject.ObjectKey == currentObject.ObjectKey {
		t.Fatal("历史定位与首次发布不符，或两份内容的对象键相同")
	}
	expectedUsed := current.FileSize + history.FileSize
	fixture.checkQuota(t, ctx, expectedUsed, 0)
	t.Logf("隔离源端同一文件 %d 保留历史 v%d 和当前 v%d，已用 %d B，预占 0 B", entryID, history.Revision, currentRevision, expectedUsed)
}

func verifyBackupSourceObjectVersion(t *testing.T, ctx context.Context, fixture replacementPublicationFixture,
	role string, objectID, revision uint64, expected []byte,
) *domain.StoredObject {
	t.Helper()
	object, err := fixture.store.FindUserStoredObject(ctx, fixture.userID, objectID)
	digest := sha256.Sum256(expected)
	if err != nil || object == nil || object.Bucket != fixture.bucket.name || object.ObjectKey == "" || object.StorageVersionID == nil ||
		*object.StorageVersionID == "" || *object.StorageVersionID == "null" || object.Status != domain.StoredObjectStatusReady ||
		object.FileSize != uint64(len(expected)) || object.VerifiedSHA256 == nil || *object.VerifiedSHA256 != digest ||
		object.UnreferencedAt != nil || object.GCAfter != nil {
		t.Fatalf("%s对象未保留已核验的版本定位、大小、摘要和有效引用，错误 %v", role, err)
	}
	version := *object.StorageVersionID
	info, err := fixture.bucket.client.StatObject(ctx, object.Bucket, object.ObjectKey, minio.StatObjectOptions{VersionID: version})
	if err != nil || info.VersionID != version || info.Size != int64(len(expected)) {
		t.Fatalf("%s对象的指定 MinIO 版本与数据库不符，错误 %v", role, err)
	}
	reader, err := fixture.bucket.client.GetObject(ctx, object.Bucket, object.ObjectKey, minio.GetObjectOptions{VersionID: version})
	if err != nil {
		t.Fatalf("读取%s正文失败 %v", role, err)
	}
	actual, readErr := io.ReadAll(io.LimitReader(reader, int64(len(expected))+1))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(actual, expected) || sha256.Sum256(actual) != *object.VerifiedSHA256 {
		t.Fatalf("%s正文的实际字节或摘要与记录不符 %v，%v", role, readErr, closeErr)
	}
	t.Logf("隔离源端%s v%d，对象 %d，版本 %s，%d B，实测 SHA-256 %x", role, revision, object.ID, version, len(actual), digest)
	return object
}
