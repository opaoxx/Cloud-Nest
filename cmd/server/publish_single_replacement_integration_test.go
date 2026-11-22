package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
)

func TestServerPublishSingleUploadReplacementIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_PUBLISH_SINGLE_REPLACEMENT_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_PUBLISH_SINGLE_REPLACEMENT_TEST=1 后使用独占 MySQL、MinIO 和服务")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	// 1. 发布原文件，记录文件 ID、版本、对象和配额基线。
	fixture := prepareReplacementPublicationFixture(t, ctx)
	t.Logf("单次替换发布专用 MySQL %s，专用 MinIO %s", fixture.mysqlContainer[:12], fixture.minioContainer[:12])
	fixture.checkQuota(t, ctx, uint64(len(fixture.original)), 0)

	// 2. 创建 SINGLE 替换任务，发送新内容并等待发布。
	const block = "CloudNest-B5!\r\n?"
	replacement := bytes.Repeat([]byte(block), int(domain.MaxSingleUploadSizeBytes)/len(block))
	digest := sha256.Sum256(replacement)
	var created dto.CreateUploadResponse
	fixture.api.request(t, ctx, http.MethodPost, fixture.entryPath+"/content-replacement", "single-replacement",
		strings.NewReader(fmt.Sprintf(`{"expected_revision":"%d","size_bytes":%d,"sha256":"%x"}`, fixture.target.Revision, len(replacement), digest)), http.StatusCreated, &created)
	taskID := backupFixtureID(t, created.Data.Task.ID)
	taskPath := "/api/v1/uploads/" + created.Data.Task.ID
	if created.Data.Task.SizeBytes != domain.MaxSingleUploadSizeBytes || created.Data.Task.Status != "UPLOADING" || created.Data.ContentPath != taskPath+"/content" {
		t.Fatal("正式创建没有返回恰好 16 MiB 的 SINGLE 路径")
	}
	fixture.checkQuota(t, ctx, uint64(len(fixture.original)), uint64(len(replacement)))
	var accepted dto.UploadContentAcceptedResponse
	fixture.api.request(t, ctx, http.MethodPut, created.Data.ContentPath, "", bytes.NewReader(replacement), http.StatusAccepted, &accepted)
	if accepted.Data.TaskID != created.Data.Task.ID || accepted.Data.Status != "VERIFYING" {
		t.Fatal("接收未交付替换任务的校验进度", accepted.Data)
	}
	waitContext, stopWait := context.WithTimeout(ctx, 25*time.Second)
	defer stopWait()
	published := awaitServerUploadPublication(t, waitContext, fixture.store, fixture.userID, taskID)

	// 3. 核对新旧内容、30 天历史留存、配额和变更事件。
	fixture.verifyPublished(t, ctx, published, replacement)
}

type replacementPublicationFixture struct {
	backupSourceFixture
	store            *repository.Store
	userID           uint64
	original         []byte
	originalObject   *domain.StoredObject
	target           *domain.FileEntry
	entryPath        string
	baselineSequence uint64
}

func prepareReplacementPublicationFixture(t *testing.T, ctx context.Context) replacementPublicationFixture {
	t.Helper()
	// 1. 在独占服务内通过普通上传发布真实的合成原文件。
	fixture := prepareBackupSource(t, ctx)
	store, err := repository.NewStore(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	userID := backupFixtureID(t, fixture.login.User.ID)
	parentID := backupFixtureID(t, fixture.login.RootEntryID)
	original := []byte("cloudnest original replacement fixture\n")
	entryID, originalObject := publishBackupSourceFile(t, ctx, fixture.api, store, fixture.bucket, userID, parentID, 0, "replacement-source.bin", original)
	target, err := store.FindUserFileEntry(ctx, userID, entryID)
	if err != nil || target == nil || target.ParentID == nil {
		t.Fatal("无法读取原文件版本", err)
	}
	var baselineSequence uint64
	if err := fixture.pool.QueryRowContext(ctx, "SELECT change_seq FROM users WHERE id=?", userID).Scan(&baselineSequence); err != nil {
		t.Fatal("无法读取原变更游标", err)
	}
	return replacementPublicationFixture{
		backupSourceFixture: fixture, store: store, userID: userID,
		original: original, originalObject: originalObject, target: target,
		entryPath: fmt.Sprintf("/api/v1/entries/%d", entryID), baselineSequence: baselineSequence,
	}
}

func (fixture replacementPublicationFixture) checkQuota(t *testing.T, ctx context.Context, wantUsed, wantReserved uint64) {
	t.Helper()
	var used, reserved uint64
	err := fixture.pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage FROM users WHERE id=?", fixture.userID).Scan(&used, &reserved)
	if err != nil || used != wantUsed || reserved != wantReserved {
		t.Fatalf("配额不符，已用 %d，预占 %d，期望 %d 和 %d，错误 %v", used, reserved, wantUsed, wantReserved, err)
	}
}

func (fixture replacementPublicationFixture) verifyPublished(t *testing.T, ctx context.Context, published *domain.UploadTask, replacement []byte) {
	t.Helper()
	// 1. 核对任务终态、同一文件的递增版本和新对象。
	target, originalObject, original := fixture.target, fixture.originalObject, fixture.original
	entryID, parentID, userID := target.ID, *target.ParentID, fixture.userID
	taskPath := fmt.Sprintf("/api/v1/uploads/%d", published.ID)
	digest := sha256.Sum256(replacement)
	revision := target.Revision + 1
	if published.ResultEntryID == nil || *published.ResultEntryID != entryID || published.PublishedObjectID == nil ||
		*published.PublishedObjectID == originalObject.ID || published.ReservedBytes != 0 ||
		published.ReplacementPublishedRevision == nil || *published.ReplacementPublishedRevision != revision {
		t.Fatal("发布结果未保留原文件标识并交付新版本")
	}
	publishedObject, err := fixture.store.FindUserStoredObject(ctx, userID, *published.PublishedObjectID)
	if err != nil || publishedObject.Status != domain.StoredObjectStatusReady || publishedObject.VerifiedSHA256 == nil ||
		*publishedObject.VerifiedSHA256 != digest || publishedObject.FileSize != uint64(len(replacement)) {
		t.Fatal("新对象未保存实测的字节数和摘要", err)
	}
	current, err := fixture.store.FindUserFileEntry(ctx, userID, entryID)
	if err != nil || current.ParentID == nil || *current.ParentID != *target.ParentID || current.ID != target.ID ||
		current.Name != target.Name || !current.CreatedAt.Equal(target.CreatedAt) || current.Revision != revision || current.FileSize != uint64(len(replacement)) {
		t.Fatal("替换没有保留文件 ID、位置、名称和创建时间，或新版本不符", err)
	}

	// 2. 按确切版本读回历史与新内容。
	var history domain.FileVersion
	err = fixture.pool.QueryRowContext(ctx, `SELECT revision,object_id,file_size,created_at,retained_until FROM file_versions
		WHERE user_id=? AND entry_id=?`, userID, entryID).
		Scan(&history.Revision, &history.ObjectID, &history.FileSize, &history.CreatedAt, &history.RetainedUntil)
	if err != nil || history.Revision != target.Revision || history.ObjectID != originalObject.ID || history.FileSize != uint64(len(original)) ||
		!history.RetainedUntil.Equal(history.CreatedAt.AddDate(0, 0, domain.DefaultFileVersionRetentionDays)) {
		t.Fatal("历史未绑定旧版本或默认保留时间不符", err)
	}
	oldStored, err := fixture.store.FindUserStoredObject(ctx, userID, history.ObjectID)
	if err != nil || oldStored.Bucket != fixture.bucket.name || oldStored.StorageVersionID == nil ||
		oldStored.Status != domain.StoredObjectStatusReady || oldStored.GCAfter != nil {
		t.Fatal("历史引用未保护旧对象版本", err)
	}
	oldReader, err := fixture.bucket.client.GetObject(ctx, fixture.bucket.name, oldStored.ObjectKey, minio.GetObjectOptions{VersionID: *oldStored.StorageVersionID})
	if err != nil {
		t.Fatal("打开旧内容失败", err)
	}
	oldBytes, readError := io.ReadAll(io.LimitReader(oldReader, int64(len(original))+1))
	closeError := oldReader.Close()
	if readError != nil || closeError != nil || !bytes.Equal(oldBytes, original) {
		t.Fatal("历史版本读回的字节不符", readError, closeError)
	}
	verifyReplacementDownload(t, ctx, fixture.backupSourceFixture, fixture.entryPath, revision, publishedObject, replacement)

	// 3. 重复完成确认后核对配额和唯一的内容替换事件。
	var completion dto.UploadTaskResponse
	for range 2 {
		fixture.api.request(t, ctx, http.MethodPost, taskPath+"/complete", "", nil, http.StatusOK, &completion)
		if completion.Data.Status != "COMPLETED" || completion.Data.ResultEntryID == nil || *completion.Data.ResultEntryID != fmt.Sprint(entryID) ||
			completion.Data.Replacement == nil || completion.Data.Replacement.PublishedRevision == nil ||
			*completion.Data.Replacement.PublishedRevision != fmt.Sprint(revision) || completion.Data.Replacement.ConflictingRevision != nil {
			t.Fatal("重复完成未交付已保存的发布版本", completion.Data)
		}
	}
	fixture.checkQuota(t, ctx, uint64(len(original)+len(replacement)), 0)
	var sequence, fileCount, versionCount, eventCount uint64
	err = fixture.pool.QueryRowContext(ctx, `SELECT change_seq,
		(SELECT COUNT(*) FROM file_entries WHERE user_id=users.id AND entry_type='FILE'),
		(SELECT COUNT(*) FROM file_versions WHERE user_id=users.id),
		(SELECT COUNT(*) FROM change_events WHERE user_id=users.id AND event_type='FILE_CONTENT_REPLACED')
		FROM users WHERE id=?`, userID).Scan(&sequence, &fileCount, &versionCount, &eventCount)
	if err != nil || sequence != fixture.baselineSequence+1 || fileCount != 1 || versionCount != 1 || eventCount != 1 {
		t.Fatal("替换的文件、历史或事件数量不符", err)
	}
	var eventEntry, eventRevision uint64
	var eventType string
	var eventBody []byte
	var payload map[string]any
	wantPayload := map[string]any{"entry_id": fmt.Sprint(entryID), "parent_id": fmt.Sprint(parentID), "name": target.Name,
		"revision": fmt.Sprint(revision), "size_bytes": float64(len(replacement)), "sha256": fmt.Sprintf("%x", digest)}
	err = fixture.pool.QueryRowContext(ctx, "SELECT entry_id,revision,event_type,payload FROM change_events WHERE user_id=? AND `sequence`=?", userID, sequence).
		Scan(&eventEntry, &eventRevision, &eventType, &eventBody)
	if err != nil || eventEntry != entryID || eventRevision != revision || eventType != domain.FileContentReplacedEventType ||
		json.Unmarshal(eventBody, &payload) != nil || !reflect.DeepEqual(payload, wantPayload) {
		t.Fatal("替换变更事件未交付实际发布的六项数据", err)
	}
	logs, err := fixture.stop()
	if err != nil {
		t.Fatal("隔离服务退出失败", err)
	}
	verifyBackupSourceStopped(t, fixture.api.baseURL, logs)
	t.Logf("文件 %d，版本 %d→%d；旧 %d B SHA-256 %x，新 %d B SHA-256 %x；已用 %d B、预占 0 B、事件序号 %d",
		entryID, target.Revision, revision, len(original), sha256.Sum256(oldBytes), len(replacement), digest, len(original)+len(replacement), sequence)
}

func verifyReplacementDownload(t *testing.T, ctx context.Context, fixture backupSourceFixture, entryPath string,
	revision uint64, object *domain.StoredObject, want []byte,
) {
	t.Helper()
	// 1. 从正式接口取得新版本的签名，只接受本轮测试桶和对象版本。
	var download dto.CreateDownloadResponse
	fixture.api.request(t, ctx, http.MethodPost, entryPath+"/download", "",
		strings.NewReader(fmt.Sprintf(`{"expected_revision":"%d","purpose":"download"}`, revision)), http.StatusOK, &download)
	expectedDigest := sha256.Sum256(want)
	endpoint, endpointError := url.Parse(os.Getenv("CLOUDNEST_S3_PUBLIC_ENDPOINT"))
	signed, signedError := url.Parse(download.Data.URL)
	if endpointError != nil || signedError != nil || signed.User != nil || signed.Scheme != endpoint.Scheme || signed.Host != endpoint.Host ||
		signed.Path != "/"+fixture.bucket.name+"/"+object.ObjectKey || object.StorageVersionID == nil ||
		signed.Query().Get("versionId") != *object.StorageVersionID || download.Data.ObjectID != fmt.Sprint(object.ID) ||
		download.Data.Revision != fmt.Sprint(revision) || download.Data.SizeBytes != uint64(len(want)) || download.Data.SHA256 != fmt.Sprintf("%x", expectedDigest) {
		t.Fatal("签名授权没有绑定本轮隔离对象和实测摘要")
	}

	// 2. 按签名独立下载，核对实际字节数和 SHA-256。
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, signed.String(), nil)
	if err != nil {
		t.Fatal("无法构造联测签名下载", err)
	}
	request.Header.Set("Accept-Encoding", "identity")
	response, err := fixture.api.httpClient.Do(request)
	if err != nil {
		t.Fatal("隔离签名下载失败")
	}
	defer response.Body.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, io.LimitReader(response.Body, int64(len(want))+1))
	if err != nil || response.StatusCode != http.StatusOK || size != int64(len(want)) || !bytes.Equal(hash.Sum(nil), expectedDigest[:]) {
		t.Fatal("新内容读回的字节数或摘要不符", response.StatusCode, size, err)
	}
}
