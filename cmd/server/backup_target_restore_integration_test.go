package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

func TestServerBackupTargetRestoreIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后验收目标回收目录恢复与下载")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	running := startRestoredBackupTarget(t, ctx)
	cancelRestoredBackupUpload(t, ctx, running)
	verifyRestoredBackupTrashDownload(t, ctx, running)
}

func verifyRestoredBackupTrashDownload(t *testing.T, ctx context.Context, running runningBackupTarget) {
	t.Helper()
	fixture, api := running.fixture, running.api
	rootID := fixture.source.login.RootEntryID
	rootBreadcrumb := dto.DirectoryBreadcrumb{ID: rootID}

	// 1. 用备份清单核对回收目录的原位置和回收时间，确认其内容尚不可浏览。
	directoryIndex := slices.IndexFunc(fixture.manifest.Directories, func(entry backupManifestEntry) bool {
		return entry.Name == "隔离备份回收目录" && entry.Status == "TRASHED"
	})
	if directoryIndex < 0 {
		t.Fatal("备份清单缺少回收目录")
	}
	folder := fixture.manifest.Directories[directoryIndex]
	folderID := strconv.FormatUint(folder.ID, 10)
	if folder.ParentID == nil || strconv.FormatUint(*folder.ParentID, 10) != rootID || folder.DeletedAt == nil {
		t.Fatal("备份清单缺少回收目录的原父目录或回收时间")
	}
	var trash dto.TrashPageResponse
	api.request(t, ctx, http.MethodGet, "/api/v1/trash", "", nil, http.StatusOK, &trash)
	if len(trash.Data.Items) != 1 || trash.Data.NextCursor != nil {
		t.Fatal("恢复前应只有一个可见回收项")
	}
	trashed := trash.Data.Items[0]
	if trashed.ID != folderID || trashed.ParentID != rootID || trashed.Name != folder.Name ||
		trashed.EntryType != "DIR" || trashed.Status != "TRASHED" ||
		trashed.Revision != strconv.FormatUint(folder.Revision, 10) || !trashed.DeletedAt.Equal(*folder.DeletedAt) ||
		!slices.Equal(trashed.OriginalPath, []dto.DirectoryBreadcrumb{rootBreadcrumb}) {
		t.Fatal("目标回收目录与备份清单不一致", trashed)
	}
	directoryPath := "/api/v1/entries?scope=directory&parent_id=" + folderID
	var hidden dto.ErrorResponse
	api.request(t, ctx, http.MethodGet, directoryPath, "", nil, http.StatusNotFound, &hidden)
	if hidden.Error.Code != "ENTRY_NOT_FOUND" {
		t.Fatal("回收目录的访问拒绝不符", hidden.Error.Code)
	}

	// 2. 用当轮读取的条目版本恢复目录，核对实际恢复位置和子文件。
	var restored dto.RestoreEntryResponse
	api.request(t, ctx, http.MethodPost, "/api/v1/trash/"+trashed.ID+"/restore", "backup-restore-directory",
		strings.NewReader(fmt.Sprintf(`{"expected_revision":%q}`, trashed.Revision)), http.StatusOK, &restored)
	entry := restored.Data.Entry
	if entry.ID != folderID || entry.ParentID != rootID || entry.Name != folder.Name ||
		entry.EntryType != "DIR" || entry.Status != "ACTIVE" || entry.Revision != strconv.FormatUint(folder.Revision+1, 10) {
		t.Fatal("正式恢复响应的实际位置或版本不符", entry)
	}
	var remainingTrash dto.TrashPageResponse
	api.request(t, ctx, http.MethodGet, "/api/v1/trash", "", nil, http.StatusOK, &remainingTrash)
	if len(remainingTrash.Data.Items) != 0 || remainingTrash.Data.NextCursor != nil {
		t.Fatal("恢复完成后回收站应为空")
	}
	var directory dto.DirectoryPageResponse
	api.request(t, ctx, http.MethodGet, directoryPath, "", nil, http.StatusOK, &directory)
	wantPath := []dto.DirectoryBreadcrumb{rootBreadcrumb, {ID: entry.ID, Name: entry.Name}}
	if !slices.Equal(directory.Data.Breadcrumbs, wantPath) || len(directory.Data.Items) != 1 || directory.Data.NextCursor != nil {
		t.Fatal("恢复后的目录路径或子项数量不符", directory.Data)
	}
	child := directory.Data.Items[0]
	childID := backupFixtureID(t, child.ID)
	childIndex := slices.IndexFunc(fixture.manifest.FileReferences, func(file backupManifestFile) bool { return file.ID == childID })
	if childIndex < 0 {
		t.Fatal("备份清单缺少该子文件", child.ID)
	}
	file := fixture.manifest.FileReferences[childIndex]
	if file.ParentID == nil || *file.ParentID != folder.ID || child.ParentID != folderID ||
		file.Name != "回收目录子文件.bin" || child.Name != file.Name || child.EntryType != "FILE" ||
		file.Status != "ACTIVE" || child.Status != file.Status || child.SizeBytes != 6 || child.SizeBytes != file.SizeBytes ||
		child.Revision != strconv.FormatUint(file.Revision, 10) {
		t.Fatal("可见子文件的归属、状态或版本与备份清单不符", child)
	}
	t.Logf("目标回收目录 %s 由版本 %s 恢复为 %s，实际父目录 %s，子文件 %s 可见", entry.ID, trashed.Revision, entry.Revision, entry.ParentID, child.ID)

	// 3. 按对象 ID 找到目标版本，经正式签名接口下载并核对内容、配额及桶内版本。
	objectIndex := slices.IndexFunc(fixture.manifest.Objects, func(object backupManifestObject) bool { return object.ID == file.ObjectID })
	locationIndex := slices.IndexFunc(fixture.mapping.Objects, func(location backupTargetObjectMapping) bool {
		return location.ObjectID == file.ObjectID
	})
	if objectIndex < 0 || locationIndex < 0 {
		t.Fatal("子文件缺少对应的备份对象或目标版本", child.ID)
	}
	object, location := fixture.manifest.Objects[objectIndex], fixture.mapping.Objects[locationIndex]
	settings, err := config.LoadStorageConfig()
	if err != nil || settings.Bucket != fixture.targetBucket.name || settings.PublicEndpointURL == nil {
		t.Fatal("无法确认隔离目标下载地址", err)
	}
	if location.TargetBucket != settings.Bucket || object.SizeBytes != file.SizeBytes ||
		object.VersionID == nil || location.TargetVersionID == *object.VersionID {
		t.Fatal("子文件的目标定位、大小或版本不符", child.ID)
	}
	verifyRestoredBackupDownload(t, ctx, api, settings.PublicEndpointURL, child, object, location)
	var profile dto.UserProfileResponse
	api.request(t, ctx, http.MethodGet, "/api/v1/me", "", nil, http.StatusOK, &profile)
	if profile.Data.UsedBytes != 10 || profile.Data.ReservedBytes != 0 {
		t.Fatal("恢复目录并下载子文件后，配额与已结算基线不符")
	}
	verifyBackupTargetVersions(t, ctx, fixture.targetBucket, fixture.mapping.Objects)
	t.Log("回收目录子文件经正式签名下载完成，目标桶保留三个对象版本，已用 10 B、预占 0 B")
}
