package main

import (
	"context"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

func TestServerBackupTargetB5RollbackDownloadIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后验证 B5 回退服务的当前文件下载")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 重建隔离回退环境，完成对象映射和离线换代后启动。
	sample := prepareBackupTargetB5RollbackSample(t, ctx)
	backupDir := t.TempDir()
	manifest := exportBackupTargetB5RollbackBundle(t, ctx, sample, backupDir)
	database := restoreBackupTargetB5RollbackDatabase(t, ctx, sample, backupDir, manifest)
	bucket, mapping := restoreBackupTargetB5RollbackObjects(t, ctx, sample, backupDir, manifest)
	verifyAndApplyBackupTargetB5RollbackObjectMap(t, ctx, sample, database, bucket, manifest, mapping)
	resetBackupTargetB5RollbackSyncDevices(t, ctx, sample, database, bucket, manifest)

	settings, err := config.LoadStorageConfig()
	if err != nil || settings.Bucket != bucket.name || settings.PublicEndpointURL == nil ||
		settings.PublicEndpointURL.Scheme != "http" || settings.PublicEndpointURL.Hostname() != "127.0.0.1" ||
		settings.PublicEndpointURL.String() != settings.EndpointURL.String() {
		t.Fatal("回退服务的签名下载需要本轮独占回环 MinIO", err)
	}
	current := manifest.FileReferences[0]
	before := readBackupB5DatabaseState(t, ctx, database.pool, current.UserID)
	var previousSessionID uint64
	if err := database.pool.QueryRowContext(ctx, "SELECT MAX(id) FROM auth_sessions WHERE user_id=?", current.UserID).
		Scan(&previousSessionID); err != nil || previousSessionID == 0 {
		t.Fatal("回退库缺少备份中已恢复的会话基线", err)
	}
	api, stop := startBackupFixtureServer(t, ctx, database.name)

	// 2. 用合成账号重新登录，从正式目录读取当前 v3。
	login := loginBackupFixture(t, ctx, &api, os.Getenv("CLOUDNEST_ADMIN_PASSWORD"))
	entryID := strconv.FormatUint(current.ID, 10)
	if login.User.ID != strconv.FormatUint(current.UserID, 10) || current.ParentID == nil ||
		login.RootEntryID != strconv.FormatUint(*current.ParentID, 10) || backupFixtureID(t, login.Session.ID) <= previousSessionID {
		t.Fatal("正式登录未给回退后的同一合成账号建立新会话")
	}
	var directory dto.DirectoryPageResponse
	api.request(t, ctx, http.MethodGet, "/api/v1/entries?scope=directory&parent_id="+login.RootEntryID,
		"", nil, http.StatusOK, &directory)
	entryIndex := slices.IndexFunc(directory.Data.Items, func(entry dto.DirectoryEntrySummary) bool {
		return entry.ID == entryID
	})
	if directory.RequestID == "" || directory.Data.NextCursor != nil || entryIndex < 0 {
		t.Fatal("回退服务根目录未返回切换后发布的当前文件")
	}
	entry := directory.Data.Items[entryIndex]
	if current.Revision != 3 || current.SizeBytes != 40 || entry.Revision != "3" || entry.SizeBytes != current.SizeBytes ||
		entry.EntryType != "FILE" || entry.Status != "ACTIVE" || entry.Name != current.Name || entry.ParentID != login.RootEntryID {
		t.Fatal("回退服务当前条目与备份的 v3 内容不符")
	}

	// 3. 申请并执行签名下载，核对正文和数据库状态。
	object, location := manifest.Objects[0], mapping.Objects[0]
	if object.ID != current.ObjectID || object.SizeBytes != current.SizeBytes || object.VersionID == nil ||
		location.ObjectID != current.ObjectID || location.TargetBucket != bucket.name || location.TargetVersionID == *object.VersionID {
		t.Fatal("当前文件未绑定回退桶的新物理版本")
	}
	verifyRestoredBackupDownload(t, ctx, api, settings.PublicEndpointURL, entry, object, location)

	var profile dto.UserProfileResponse
	api.request(t, ctx, http.MethodGet, "/api/v1/me", "", nil, http.StatusOK, &profile)
	if profile.RequestID == "" || profile.Data.User.ID != login.User.ID || profile.Data.RootEntryID != login.RootEntryID ||
		profile.Data.UsedBytes != 112 || profile.Data.ReservedBytes != 0 {
		t.Fatal("回退服务交付的账号、根目录或配额不符")
	}
	if state := readBackupB5DatabaseState(t, ctx, database.pool, current.UserID); !reflect.DeepEqual(state, before) ||
		state.high != 5 || state.used != 112 || state.reserved != 0 {
		t.Fatal("回退启动和下载后日志、设备同步状态或配额未保持一致")
	}
	logs, err := stop()
	if err != nil {
		t.Fatal("隔离回退服务关停失败", err)
	}
	verifyBackupB5ServiceStopped(t, api.baseURL, logs)
	t.Logf("B5 回退服务签名下载，条目 %s 修订 %s，回退版本 %s，%d B，SHA-256 %s，保留日志 1～5，已用 112 B，预占 0 B",
		entry.ID, entry.Revision, location.TargetVersionID, object.SizeBytes, object.SHA256)
	t.Logf("本轮专用容器 MySQL %s，MinIO %s，后端已关停", sample.fixture.source.mysqlContainer, sample.fixture.source.minioContainer)
}
