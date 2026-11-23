package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
)

func TestServerB5LocalDeletionConflictIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后验证 B5 本地删除与远端修改冲突")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 发布原文件，保存并确认双方一致的基线。
	fixture := prepareBackupSource(t, ctx)
	userID := backupFixtureID(t, fixture.login.User.ID)
	rootID := backupFixtureID(t, fixture.login.RootEntryID)
	store, err := repository.NewStore(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	const fileName = "local-deletion-conflict.txt"
	original := []byte("content from device v1\n")
	replacement := []byte("content from device v2\n")
	digest := sha256.Sum256(replacement)
	if len(original) != len(replacement) || sha256.Sum256(original) == digest {
		t.Fatal("合成版本应具有相同字节数和不同摘要")
	}
	entryID, originalObject := publishBackupSourceFile(t, ctx, fixture.api, store, fixture.bucket, userID, rootID, 1, fileName, original)
	entryPath := fmt.Sprintf("/api/v1/entries/%d", entryID)
	local, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := local.Close(); err != nil {
			t.Error("关闭本地合成目录失败", err)
		}
	})
	if err := local.WriteFile(fileName, original, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot := establishB5DeletionTestBaseline(t, ctx, fixture.api, local, fixture.login.RootEntryID, 1)

	// 2. 由另一设备替换正文，再让本地缺项进入删除传播检查。
	otherAPI := fixture.api
	otherLogin := loginBackupFixture(t, ctx, &otherAPI, os.Getenv("CLOUDNEST_ADMIN_PASSWORD"))
	if otherLogin.User.ID != fixture.login.User.ID || otherLogin.Device.ID == "" || otherLogin.Device.ID == fixture.login.Device.ID {
		t.Fatal("远端替换应由同一合成账号的另一设备执行")
	}
	var created dto.CreateUploadResponse
	otherAPI.request(t, ctx, http.MethodPost, entryPath+"/content-replacement", "b5-deletion-conflict-replace",
		strings.NewReader(fmt.Sprintf(`{"expected_revision":"1","size_bytes":%d,"sha256":"%x"}`, len(replacement), digest)),
		http.StatusCreated, &created)
	taskID := backupFixtureID(t, created.Data.Task.ID)
	if created.Data.ContentPath != "/api/v1/uploads/"+created.Data.Task.ID+"/content" {
		t.Fatal("替换任务应返回其专属的 SINGLE 正文地址")
	}
	sendSingleReplacementContent(t, ctx, otherAPI, created.Data, replacement)
	waitContext, stopWait := context.WithTimeout(ctx, 25*time.Second)
	defer stopWait()
	published := awaitServerUploadPublication(t, waitContext, store, userID, taskID)
	if published.ResultEntryID == nil || *published.ResultEntryID != entryID ||
		published.ReplacementPublishedRevision == nil || *published.ReplacementPublishedRevision != 2 ||
		published.PublishedObjectID == nil || *published.PublishedObjectID == originalObject.ID {
		t.Fatal("另一设备应在原条目发布新对象与修订 2")
	}
	current, err := store.FindUserFileEntry(ctx, userID, entryID)
	if err != nil || current == nil || current.Status != "ACTIVE" || current.Revision != 2 ||
		current.ParentID == nil || *current.ParentID != rootID || current.Name != fileName || current.FileSize != uint64(len(original)) {
		t.Fatal("远端文件应只更换正文及修订，保留原位置、名称和字节数", err)
	}
	before := readBackupB5DatabaseState(t, ctx, fixture.pool, userID)
	if before.high != 2 || len(before.events) != 2 || before.events[0].eventType != domain.FilePublishedEventType ||
		before.events[1].eventType != domain.FileContentReplacedEventType || before.events[1].sequence != 2 ||
		before.events[1].entryID != entryID || before.events[1].revision != 2 ||
		len(before.devices) != 1 || before.devices[0].DeviceID != backupFixtureID(t, fixture.login.Device.ID) ||
		before.devices[0].State.Status != domain.SyncDeviceStatusActive || before.devices[0].State.AckSequence != 1 ||
		before.used != uint64(len(original)+len(replacement)) || before.reserved != 0 {
		t.Fatal("远端应已发布修改，原设备仍处于确认位置 1")
	}
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
	if len(missing) != 1 || missing[0].EntryID != fmt.Sprint(entryID) || missing[0].Revision != "1" {
		t.Fatal("新远端修订不应覆盖客户端用于缺项判断的原基线")
	}

	// 3. 核对客户端只读取详情，并下载远端新正文验证 SHA-256。
	if recalled := client.propagateLocalDeletion(t, ctx, missing[0]); recalled != nil {
		t.Fatal("远端已修改时应暂停回收", recalled)
	}
	if want := []string{"GET " + entryPath}; !reflect.DeepEqual(recorder.requests, want) {
		t.Fatal("删除传播检查只应发送一次详情读取", recorder.requests)
	}
	var trash dto.TrashPageResponse
	fixture.api.request(t, ctx, http.MethodGet, "/api/v1/trash", "", nil, http.StatusOK, &trash)
	after := readBackupB5DatabaseState(t, ctx, fixture.pool, userID)
	if trash.RequestID == "" || len(trash.Data.Items) != 0 || !reflect.DeepEqual(after, before) {
		t.Fatal("复核应保留空回收站、变更日志、确认位置及配额")
	}
	newObject, err := store.FindUserStoredObject(ctx, userID, *published.PublishedObjectID)
	if err != nil || newObject == nil {
		t.Fatal("远端新版本缺少可读对象", err)
	}
	verifyReplacementDownload(t, ctx, fixture, entryPath, 2, newObject, replacement)
	logs, err := fixture.stop()
	if err != nil {
		t.Fatal("隔离同步服务关停失败", err)
	}
	verifyBackupB5ServiceStopped(t, fixture.api.baseURL, logs)
	t.Logf("B5 本地基线修订 1 与远端修订 2 冲突，条目 %d 只读取详情且未回收，确认位置 1，新正文 %d B SHA-256 %x，已用 %d B",
		entryID, len(replacement), digest, after.used)
	t.Logf("本轮专用容器 MySQL %s，MinIO %s，后端已关停", fixture.mysqlContainer, fixture.minioContainer)
}
