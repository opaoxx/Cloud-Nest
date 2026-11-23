package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

func TestServerBackupTargetB5CurrentDownloadIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后验收恢复目标的 B5 当前文件下载")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 恢复隔离目标，完成对象定位和离线换代。
	fixture := prepareBackupTargetB5Remap(t, ctx)
	verifyAndApplyBackupTargetB5ObjectMap(t, ctx, fixture)
	resetBackupTargetB5SyncDevices(t, ctx, fixture)
	settings, err := config.LoadStorageConfig()
	if err != nil || settings.Bucket != fixture.targetBucket.name || settings.PublicEndpointURL == nil ||
		settings.PublicEndpointURL.Scheme != "http" || settings.PublicEndpointURL.Hostname() != "127.0.0.1" ||
		settings.PublicEndpointURL.String() != settings.EndpointURL.String() {
		t.Fatal("正式下载验收需要独占目标桶和同一个回环 MinIO 地址", err)
	}
	target, stopTarget := startBackupFixtureServer(t, ctx, fixture.database.name)
	sourceLogin := fixture.source.login
	login := loginBackupFixture(t, ctx, &target, os.Getenv("CLOUDNEST_ADMIN_PASSWORD"))
	if login.User.ID != sourceLogin.User.ID || login.RootEntryID != sourceLogin.RootEntryID ||
		login.Session.ID == sourceLogin.Session.ID {
		t.Fatal("目标合成账号、目录或独立登录会话不符")
	}
	userID := backupFixtureID(t, login.User.ID)
	manifest := fixture.manifest
	current, history := manifest.FileReferences[0], manifest.HistoryReferences[0]
	if current.ParentID == nil || fmt.Sprint(*current.ParentID) != login.RootEntryID || current.UserID != userID ||
		current.ID != history.EntryID || history.Revision != 1 || current.Revision != 2 ||
		history.SizeBytes != 39 || current.SizeBytes != 33 || current.ObjectID == history.ObjectID {
		t.Fatal("备份应包含同一根目录文件的当前 v2 和历史 v1")
	}
	before := readBackupB5DatabaseState(t, ctx, fixture.database.pool, userID)

	// 2. 从正式目录取得当前修订，核对旧修订请求返回冲突。
	var directory dto.DirectoryPageResponse
	target.request(t, ctx, http.MethodGet, "/api/v1/entries?scope=directory&parent_id="+login.RootEntryID,
		"", nil, http.StatusOK, &directory)
	entryID := strconv.FormatUint(current.ID, 10)
	entryIndex := slices.IndexFunc(directory.Data.Items, func(entry dto.DirectoryEntrySummary) bool {
		return entry.ID == entryID
	})
	if directory.RequestID == "" || directory.Data.NextCursor != nil || entryIndex < 0 {
		t.Fatal("正式根目录未返回备份中的当前文件")
	}
	entry := directory.Data.Items[entryIndex]
	if entry.ParentID != login.RootEntryID || entry.EntryType != "FILE" || entry.Status != "ACTIVE" ||
		entry.Name != current.Name || entry.Revision != strconv.FormatUint(current.Revision, 10) || entry.SizeBytes != current.SizeBytes {
		t.Fatal("恢复后目录项的名称、修订或长度与当前文件引用不符")
	}
	var conflict dto.ErrorResponse
	target.request(t, ctx, http.MethodPost, "/api/v1/entries/"+entry.ID+"/download", "",
		strings.NewReader(fmt.Sprintf(`{"expected_revision":%q,"purpose":"download"}`, strconv.FormatUint(history.Revision, 10))),
		http.StatusConflict, &conflict)
	if conflict.RequestID == "" || conflict.Error.Code != "REVISION_CONFLICT" {
		t.Fatal("普通文件下载应按契约拒绝已过期的修订")
	}

	// 3. 用当前修订申请下载，核对目标对象版本、字节数和 SHA-256。
	objectIndex := slices.IndexFunc(manifest.Objects, func(object backupManifestObject) bool {
		return object.ID == current.ObjectID
	})
	locationIndex := slices.IndexFunc(fixture.mapping.Objects, func(location backupTargetObjectMapping) bool {
		return location.ObjectID == current.ObjectID
	})
	if objectIndex < 0 || locationIndex < 0 {
		t.Fatal("当前文件缺少备份对象或已恢复的物理版本映射")
	}
	object, location := manifest.Objects[objectIndex], fixture.mapping.Objects[locationIndex]
	if object.SizeBytes != current.SizeBytes || object.VersionID == nil ||
		location.TargetVersionID == "" || location.TargetVersionID == *object.VersionID || location.TargetBucket != fixture.targetBucket.name {
		t.Fatal("下载应使用本次恢复写入的目标版本")
	}
	verifyRestoredBackupDownload(t, ctx, target, settings.PublicEndpointURL, entry, object, location)

	var profile dto.UserProfileResponse
	target.request(t, ctx, http.MethodGet, "/api/v1/me", "", nil, http.StatusOK, &profile)
	used := current.SizeBytes + history.SizeBytes
	if profile.RequestID == "" || profile.Data.User.ID != login.User.ID || profile.Data.UsedBytes != used || profile.Data.ReservedBytes != 0 {
		t.Fatal("下载后的公开配额应包含当前与历史共 72 B，预占为 0 B")
	}
	if after := readBackupB5DatabaseState(t, ctx, fixture.database.pool, userID); !reflect.DeepEqual(after, before) ||
		after.high != 4 || after.used != used || after.reserved != 0 {
		t.Fatal("下载请求改变了目标的同步状态、保留日志或配额")
	}
	logs, err := stopTarget()
	if err != nil {
		t.Fatal("隔离目标关停失败", err)
	}
	verifyBackupSourceStopped(t, target.baseURL, logs)
	t.Logf("B5 恢复下载验收，条目 %s 当前修订 %s，独立目标版本 %s，原修订请求 409 REVISION_CONFLICT，当前正文 %d B SHA-256 %s，已用 %d B，预占 0 B",
		entry.ID, entry.Revision, location.TargetVersionID, object.SizeBytes, object.SHA256, used)
	t.Logf("本轮专用容器 MySQL %s，MinIO %s，后端已关停", fixture.source.mysqlContainer, fixture.source.minioContainer)
}
