package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

func TestServerBackupTargetDownloadIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后验收恢复目标的签名下载")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 恢复隔离目标，完成未发送任务的收尾，再从正式根目录读取待下载文件。
	running := startRestoredBackupTarget(t, ctx)
	cancelRestoredBackupUpload(t, ctx, running)
	fixture, api := running.fixture, running.api
	rootID := backupFixtureID(t, fixture.source.login.RootEntryID)
	var directory dto.DirectoryPageResponse
	api.request(t, ctx, http.MethodGet, "/api/v1/entries?scope=directory&parent_id="+strconv.FormatUint(rootID, 10),
		"", nil, http.StatusOK, &directory)
	files := make(map[uint64]backupManifestFile)
	for _, file := range fixture.manifest.FileReferences {
		if file.ParentID != nil && *file.ParentID == rootID && file.Status == "ACTIVE" {
			files[file.ID] = file
		}
	}
	if len(files) != 2 || len(directory.Data.Items) != len(files) || directory.Data.NextCursor != nil {
		t.Fatal("恢复后的根目录应包含活动文件与空文件")
	}

	// 2. 用对象 ID 关联备份内容摘要与目标版本，校验目录项并实际下载。
	objects := make(map[uint64]backupManifestObject, len(fixture.manifest.Objects))
	for _, object := range fixture.manifest.Objects {
		objects[object.ID] = object
	}
	locations := make(map[uint64]backupTargetObjectMapping, len(fixture.mapping.Objects))
	for _, location := range fixture.mapping.Objects {
		locations[location.ObjectID] = location
	}
	settings, err := config.LoadStorageConfig()
	if err != nil || settings.Bucket != fixture.targetBucket.name || settings.PublicEndpointURL == nil {
		t.Fatal("无法确认隔离目标下载地址", err)
	}
	for _, entry := range directory.Data.Items {
		entryID := backupFixtureID(t, entry.ID)
		file, ok := files[entryID]
		if !ok || entry.Name != file.Name || entry.Status != "ACTIVE" || entry.EntryType != "FILE" ||
			entry.ParentID != strconv.FormatUint(rootID, 10) || entry.SizeBytes != file.SizeBytes ||
			entry.Revision != strconv.FormatUint(file.Revision, 10) {
			t.Fatal("目标目录的文件事实与备份清单不一致", entry.ID)
		}
		object, hasObject := objects[file.ObjectID]
		location, hasLocation := locations[file.ObjectID]
		if !hasObject || !hasLocation || object.SizeBytes != file.SizeBytes || location.TargetBucket != settings.Bucket {
			t.Fatal("目录项缺少对应的恢复对象或目标版本", entry.ID)
		}
		verifyRestoredBackupDownload(t, ctx, api, settings.PublicEndpointURL, entry, object, location)
		delete(files, entryID)
	}

	// 3. 核对下载后的配额和已发布版本，保留后续回收恢复的基线。
	var profile dto.UserProfileResponse
	api.request(t, ctx, http.MethodGet, "/api/v1/me", "", nil, http.StatusOK, &profile)
	if profile.Data.UsedBytes != 10 || profile.Data.ReservedBytes != 0 {
		t.Fatal("下载后的配额与已结算基线不符")
	}
	verifyBackupTargetVersions(t, ctx, fixture.targetBucket, fixture.mapping.Objects)
	t.Log("两份活动文件经正式签名下载完成，目标桶保留三个对象版本，已用 10 B、预占 0 B")
}

func verifyRestoredBackupDownload(t *testing.T, ctx context.Context, api backupSourceClient, endpoint *url.URL,
	entry dto.DirectoryEntrySummary, object backupManifestObject, location backupTargetObjectMapping) {
	t.Helper()
	// 1. 通过目标后端申请下载链接，核对授权对象、目录项版本及内容摘要。
	var grant dto.CreateDownloadResponse
	api.request(t, ctx, http.MethodPost, "/api/v1/entries/"+entry.ID+"/download", "",
		strings.NewReader(fmt.Sprintf(`{"expected_revision":%q,"purpose":"download"}`, entry.Revision)), http.StatusOK, &grant)
	if grant.Data.ObjectID != strconv.FormatUint(object.ID, 10) || grant.Data.Revision != entry.Revision ||
		grant.Data.SizeBytes != object.SizeBytes || grant.Data.SHA256 != object.SHA256 {
		t.Fatal("目标后端的下载授权与恢复对象不符", entry.ID)
	}

	// 2. 核对签名指向本轮隔离目标的桶、原对象键和目标版本。
	signed, err := url.Parse(grant.Data.URL)
	if err != nil || signed.User != nil || signed.Host != endpoint.Host || signed.Scheme != endpoint.Scheme ||
		signed.Path != "/"+location.TargetBucket+"/"+location.ObjectKey || signed.Query().Get("versionId") != location.TargetVersionID {
		t.Fatal("签名链接没有绑定恢复后的目标桶和版本", entry.ID)
	}

	// 3. 直接访问签名链接，核对实际响应及字节摘要。该请求只使用 URL 提供的签名。
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, grant.Data.URL, nil)
	if err != nil {
		t.Fatal("无法构造目标签名下载请求", entry.ID)
	}
	request.Header.Set("Accept-Encoding", "identity")
	response, err := api.httpClient.Do(request)
	if err != nil {
		t.Fatal("访问目标签名链接失败", entry.ID)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength != int64(object.SizeBytes) ||
		response.Header.Get("X-Amz-Version-Id") != location.TargetVersionID ||
		strings.Trim(response.Header.Get("ETag"), `"`) != location.TargetETag {
		t.Fatalf("目标 MinIO 下载响应不符，条目 %s，HTTP %d，长度 %d", entry.ID, response.StatusCode, response.ContentLength)
	}
	digest := sha256.New()
	size, err := io.Copy(digest, io.LimitReader(response.Body, int64(object.SizeBytes)+1))
	if err != nil || size != int64(object.SizeBytes) || fmt.Sprintf("%x", digest.Sum(nil)) != object.SHA256 {
		t.Fatal("目标下载正文与备份清单不一致", entry.ID, size, err)
	}
	t.Logf("目标签名下载 %s，条目 %s，版本 %s，%d B，SHA-256 %x", entry.Name, entry.ID, location.TargetVersionID, size, digest.Sum(nil))
}
