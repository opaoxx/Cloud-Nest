package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
)

func TestServerB5EnabledLocalDeletionPropagationIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后验证 B5 显式开启本地删除传播")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 发布合成文件，保存并确认双方一致的基线。
	fixture := prepareBackupSource(t, ctx)
	userID := backupFixtureID(t, fixture.login.User.ID)
	rootID := backupFixtureID(t, fixture.login.RootEntryID)
	store, err := repository.NewStore(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	const fileName = "opt-in-local-deletion.txt"
	body := []byte("retained by conditional trash\n")
	entryID, object := publishBackupSourceFile(t, ctx, fixture.api, store, fixture.bucket, userID, rootID, 1, fileName, body)
	local, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := local.Close(); err != nil {
			t.Error("关闭本地合成目录失败", err)
		}
	})
	if err := local.WriteFile(fileName, body, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot := establishB5DeletionTestBaseline(t, ctx, fixture.api, local, fixture.login.RootEntryID, 1)
	before := readBackupB5DatabaseState(t, ctx, fixture.pool, userID)
	if before.high != 1 || len(before.events) != 1 || before.events[0].eventType != domain.FilePublishedEventType ||
		len(before.devices) != 1 || before.devices[0].DeviceID != backupFixtureID(t, fixture.login.Device.ID) ||
		before.devices[0].State.Status != domain.SyncDeviceStatusActive || before.devices[0].State.AckSequence != 1 ||
		before.used != uint64(len(body)) || before.reserved != 0 {
		t.Fatal("隔离客户端尚未确认文件发布基线")
	}

	// 2. 显式开启传播，复核本地缺项和远端修订后请求回收。
	if err := local.Remove(fileName); err != nil {
		t.Fatal("无法移除本轮临时目录的合成文件", err)
	}
	recorder := &b5DeletionRequestRecorder{transport: http.DefaultTransport}
	api := fixture.api
	api.httpClient = &http.Client{
		Transport: recorder, Timeout: fixture.api.httpClient.Timeout, CheckRedirect: fixture.api.httpClient.CheckRedirect,
	}
	client := b5DeletionTestClient{api: api, local: local, propagateLocalDeletes: true}
	missing := client.missingLocalEntries(t, snapshot)
	if len(missing) != 1 || missing[0].EntryID != strconv.FormatUint(entryID, 10) || missing[0].Revision != "1" {
		t.Fatal("本地缺项未与已保存的原文件及修订对应")
	}
	recall := client.propagateLocalDeletion(t, ctx, missing[0])
	if recall == nil || recall.ID != missing[0].EntryID || recall.Revision != "2" || recall.Status != "TRASHED" ||
		recall.ParentID != fixture.login.RootEntryID || recall.Name != fileName || recall.EntryType != "FILE" {
		t.Fatal("显式开启后，正式条件回收未交付同一文件的新修订")
	}

	// 3. 核对请求轨迹、回收事件和仍保留的物理对象。
	entryPath := "/api/v1/entries/" + missing[0].EntryID
	if want := []string{"GET " + entryPath, "POST " + entryPath + "/trash"}; !reflect.DeepEqual(recorder.requests, want) {
		t.Fatal("客户端应先查询详情，再且仅发送一次条件回收", recorder.requests)
	}
	var trash dto.TrashPageResponse
	fixture.api.request(t, ctx, http.MethodGet, "/api/v1/trash", "", nil, http.StatusOK, &trash)
	if trash.RequestID == "" || len(trash.Data.Items) != 1 ||
		trash.Data.Items[0].TrashedEntrySummary != *recall || trash.Data.Items[0].SizeBytes != uint64(len(body)) {
		t.Fatal("正式回收站没有保留预期文件记录")
	}
	after := readBackupB5DatabaseState(t, ctx, fixture.pool, userID)
	if after.high != 2 || len(after.events) != 2 || !reflect.DeepEqual(after.events[:1], before.events) ||
		after.events[1].sequence != 2 || after.events[1].entryID != entryID || after.events[1].revision != 2 ||
		after.events[1].eventType != "ENTRY_TRASHED" || !reflect.DeepEqual(after.devices, before.devices) ||
		after.used != before.used || after.reserved != before.reserved {
		t.Fatal("回收后应只追加一条墓碑，保留基线确认位置和配额")
	}
	var storedObjectID uint64
	if err := fixture.pool.QueryRowContext(ctx, "SELECT object_id FROM file_entries WHERE user_id=? AND id=? AND status='TRASHED'", userID, entryID).
		Scan(&storedObjectID); err != nil || storedObjectID != object.ID {
		t.Fatal("回收条目未保留其原对象引用", err)
	}
	retained, err := store.FindUserStoredObject(ctx, userID, object.ID)
	if err != nil || !reflect.DeepEqual(retained, object) {
		t.Fatal("条件回收改变了原物理对象版本或计时", err)
	}
	version := *object.StorageVersionID
	physical, err := fixture.bucket.client.StatObject(ctx, object.Bucket, object.ObjectKey, minio.StatObjectOptions{VersionID: version})
	if err != nil || physical.VersionID != version || physical.Size != int64(len(body)) {
		t.Fatal("软删除后原物理版本应仍能从隔离桶定位", err)
	}
	logs, err := fixture.stop()
	if err != nil {
		t.Fatal("隔离同步服务关停失败", err)
	}
	verifyBackupB5ServiceStopped(t, fixture.api.baseURL, logs)
	t.Logf("B5 显式开启本地删除传播，条目 %d 按基线修订 1 条件回收至 2，仅一次回收请求，墓碑序号 2，确认位置 1，保留对象 %d 及已用 %d B",
		entryID, object.ID, after.used)
	t.Logf("本轮专用容器 MySQL %s，MinIO %s，后端已关停", fixture.mysqlContainer, fixture.minioContainer)
}

func (client b5DeletionTestClient) propagateLocalDeletion(t *testing.T, ctx context.Context, baseline dto.SyncSnapshotEntry) *dto.TrashedEntrySummary {
	t.Helper()
	// 1. 仅在测试客户端显式开启后，只读复核远端仍处于原基线。
	if !client.propagateLocalDeletes {
		return nil
	}
	entryPath := "/api/v1/entries/" + baseline.EntryID
	var detail dto.EntryDetailResponse
	client.api.request(t, ctx, http.MethodGet, entryPath, "", nil, http.StatusOK, &detail)
	remote := detail.Data
	if detail.RequestID == "" || remote.Status != "ACTIVE" || remote.ID != baseline.EntryID ||
		remote.ParentID == nil || *remote.ParentID != baseline.ParentID || remote.Name != baseline.Name ||
		remote.Revision != baseline.Revision || remote.SizeBytes != baseline.SizeBytes {
		return nil
	}

	// 2. 再检查本地文件仍缺失，避免传播扫描后已经恢复的路径。
	if _, err := client.local.Stat(baseline.Name); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal("复核本地合成文件失败", err)
	}

	// 3. 用基线修订请求正式条件回收，由后端在事务内再次检查。
	var trashed dto.TrashEntryResponse
	client.api.request(t, ctx, http.MethodPost, entryPath+"/trash", "b5-local-deletion-"+baseline.EntryID+"-"+baseline.Revision,
		strings.NewReader(fmt.Sprintf(`{"expected_revision":%q}`, baseline.Revision)), http.StatusOK, &trashed)
	if trashed.RequestID == "" {
		t.Fatal("条件回收未交付请求 ID")
	}
	return &trashed.Data.Entry
}
