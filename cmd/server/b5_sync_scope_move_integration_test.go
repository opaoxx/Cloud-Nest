package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
)

func TestServerB5SyncScopeMoveIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后验证 B5 文件移出同步根")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 发布合成文件，保存并确认包含两个目录的基线。
	fixture := prepareBackupSource(t, ctx)
	userID := backupFixtureID(t, fixture.login.User.ID)
	accountRootID := fixture.login.RootEntryID
	store, err := repository.NewStore(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	createDirectory := func(name string) string {
		t.Helper()
		var created dto.CreateDirectoryResponse
		fixture.api.request(t, ctx, http.MethodPost, "/api/v1/directories", "b5-scope-"+name,
			strings.NewReader(fmt.Sprintf(`{"parent_id":%q,"name":%q}`, accountRootID, name)), http.StatusCreated, &created)
		if created.RequestID == "" || created.Data.Entry.ID == "" || created.Data.Entry.Name != name {
			t.Fatal("未创建本轮合成目录", name)
		}
		return created.Data.Entry.ID
	}
	selectedRootID := createDirectory("selected-scope")
	outsideRootID := createDirectory("outside-scope")
	const fileName = "scope-move.txt"
	body := []byte("content retained outside the sync root\n")
	entryID, object := publishBackupSourceFile(t, ctx, fixture.api, store, fixture.bucket,
		userID, backupFixtureID(t, selectedRootID), 1, fileName, body)
	entryIDText := fmt.Sprint(entryID)
	entryPath := "/api/v1/entries/" + entryIDText
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
	const snapshotPath = "/api/v1/sync/snapshots"
	var snapshot dto.SyncSnapshotPageResponse
	fixture.api.request(t, ctx, http.MethodPost, snapshotPath, "b5-scope-move-baseline",
		strings.NewReader("{}"), http.StatusCreated, &snapshot)
	if snapshot.RequestID == "" || snapshot.Data.RootEntryID != accountRootID || snapshot.Data.SnapshotID == "" ||
		snapshot.Data.HighWaterSequence != "3" || snapshot.Data.NextCursor != nil || snapshot.Data.BaselineToken == nil ||
		*snapshot.Data.BaselineToken == "" || len(snapshot.Data.Items) != 3 {
		t.Fatal("完整基线应包含两个普通目录和一个文件", snapshot.Data.HighWaterSequence)
	}
	var saved dto.SyncSnapshotPageData
	saveAndReadB5DeletionFixtureJSON(t, local, "baseline.json", snapshot.Data, &saved)
	if !reflect.DeepEqual(saved, snapshot.Data) {
		t.Fatal("读回基线与快照交付不一致")
	}
	entries := make(map[string]dto.SyncSnapshotEntry, len(saved.Items))
	var scopedFile json.RawMessage
	for _, raw := range saved.Items {
		var entry dto.SyncSnapshotEntry
		if err := json.Unmarshal(raw, &entry); err != nil {
			t.Fatal("无法读取已保存的基线条目", err)
		}
		entries[entry.EntryID] = entry
		if entry.EntryID == entryIDText {
			scopedFile = raw
		}
	}
	selected, outside, file := entries[selectedRootID], entries[outsideRootID], entries[entryIDText]
	localBody, err := local.ReadFile(fileName)
	if err != nil || selected.EntryType != "DIR" || outside.EntryType != "DIR" ||
		selected.ParentID != accountRootID || outside.ParentID != accountRootID || selectedRootID == outsideRootID ||
		file.EntryType != "FILE" || file.ParentID != selectedRootID || file.Revision != "1" || file.Name != fileName ||
		file.SizeBytes != uint64(len(localBody)) || file.SHA256 == nil || *file.SHA256 != fmt.Sprintf("%x", sha256.Sum256(localBody)) {
		t.Fatal("基线须明确选中同步根、其同级目录及已匹配正文", err)
	}
	var baselineAck dto.ConfirmSyncSnapshotBaselineResponse
	fixture.api.request(t, ctx, http.MethodPost, snapshotPath+"/"+saved.SnapshotID+"/confirm", "",
		strings.NewReader(fmt.Sprintf(`{"baseline_token":%q}`, *saved.BaselineToken)), http.StatusOK, &baselineAck)
	if baselineAck.RequestID == "" || baselineAck.Data.Status != "ACTIVE" || baselineAck.Data.AckSequence != "3" {
		t.Fatal("原设备未确认已保存的基线")
	}

	// 2. 由另一设备移出文件，拉取移动事件并处理本地原路径缺项。
	otherAPI := fixture.api
	otherLogin := loginBackupFixture(t, ctx, &otherAPI, os.Getenv("CLOUDNEST_ADMIN_PASSWORD"))
	if otherLogin.User.ID != fixture.login.User.ID || otherLogin.Device.ID == "" || otherLogin.Device.ID == fixture.login.Device.ID {
		t.Fatal("移动应由同一合成账号的另一设备执行")
	}
	var moved dto.MoveEntryResponse
	otherAPI.request(t, ctx, http.MethodPost, entryPath+"/move", "b5-move-outside-sync-scope",
		strings.NewReader(fmt.Sprintf(`{"target_parent_id":%q,"expected_revision":"1"}`, outsideRootID)), http.StatusOK, &moved)
	if moved.RequestID == "" || moved.Data.Entry.ID != entryIDText || moved.Data.Entry.Revision != "2" ||
		moved.Data.Entry.Status != "ACTIVE" || moved.Data.Entry.ParentID == nil || *moved.Data.Entry.ParentID != outsideRootID {
		t.Fatal("正式移动应将原文件推进到目标目录的修订 2")
	}
	if err := local.Rename(fileName, "retained-scope-move.txt"); err != nil {
		t.Fatal("无法在隔离目录保留合成文件并制造原路径缺项", err)
	}
	recorder := &b5DeletionRequestRecorder{transport: http.DefaultTransport}
	api := fixture.api
	api.httpClient = &http.Client{
		Transport: recorder, Timeout: fixture.api.httpClient.Timeout, CheckRedirect: fixture.api.httpClient.CheckRedirect,
	}
	client := b5DeletionTestClient{api: api, local: local, propagateLocalDeletes: true}
	missing := client.missingLocalEntries(t, dto.SyncSnapshotPageData{Items: []json.RawMessage{scopedFile}})
	if len(missing) != 1 || missing[0].EntryID != entryIDText || missing[0].Revision != "1" {
		t.Fatal("已确认范围内应只发现一条原路径缺项")
	}
	const pullPath = "/api/v1/sync/changes/pull"
	var page dto.PullSyncChangesResponse
	api.request(t, ctx, http.MethodPost, pullPath, "",
		strings.NewReader(fmt.Sprintf(`{"baseline_token":%q}`, *saved.BaselineToken)), http.StatusOK, &page)
	if page.RequestID == "" || page.Data.HasMore || page.Data.HighWaterSequence != "4" ||
		page.Data.NextCursor == "" || len(page.Data.Events) != 1 || page.Data.Events[0].Sequence != "4" ||
		page.Data.Events[0].Revision != "2" ||
		!client.reconcileB5MissingFileAfterSiblingMove(t, ctx, missing[0], page.Data.Events[0], selected, outside) {
		t.Fatal("测试客户端未识别原条目移出所选同步根")
	}

	// 3. 保存结果并确认游标，核对没有回收请求且移动后正文可读。
	beforeConfirm := readBackupB5DatabaseState(t, ctx, fixture.pool, userID)
	if beforeConfirm.high != 4 || len(beforeConfirm.events) != 4 || beforeConfirm.events[3].eventType != "ENTRY_MOVED" ||
		beforeConfirm.events[3].entryID != entryID || beforeConfirm.events[3].revision != 2 ||
		len(beforeConfirm.devices) != 1 || beforeConfirm.devices[0].DeviceID != backupFixtureID(t, fixture.login.Device.ID) ||
		beforeConfirm.devices[0].State.AckSequence != 3 || beforeConfirm.used != uint64(len(body)) || beforeConfirm.reserved != 0 {
		t.Fatal("拉取后应只有移动事件，确认位置及配额应维持基线")
	}
	var checkpoint dto.PullSyncChangesData
	saveAndReadB5DeletionFixtureJSON(t, local, "scope-move-checkpoint.json", page.Data, &checkpoint)
	if !reflect.DeepEqual(checkpoint, page.Data) {
		t.Fatal("移动事件和游标未完整保存")
	}
	const confirmPath = "/api/v1/sync/changes/confirm"
	var confirmed dto.ConfirmSyncSnapshotBaselineResponse
	api.request(t, ctx, http.MethodPost, confirmPath, "",
		strings.NewReader(fmt.Sprintf(`{"cursor":%q}`, checkpoint.NextCursor)), http.StatusOK, &confirmed)
	if confirmed.RequestID == "" || confirmed.Data.Status != "ACTIVE" || confirmed.Data.AckSequence != "4" ||
		!reflect.DeepEqual(recorder.requests, []string{"POST " + pullPath, "POST " + confirmPath}) {
		t.Fatal("原设备只应拉取并确认已保存的移动事件", recorder.requests)
	}
	beforeConfirm.devices[0].State.AckSequence = 4
	if after := readBackupB5DatabaseState(t, ctx, fixture.pool, userID); !reflect.DeepEqual(after, beforeConfirm) {
		t.Fatal("确认移动事件应只推进设备位置")
	}
	if retained, err := local.ReadFile("retained-scope-move.txt"); err != nil || !bytes.Equal(retained, body) {
		t.Fatal("本地合成正文应仍可读", err)
	}
	verifyReplacementDownload(t, ctx, fixture, entryPath, 2, object, body)
	logs, err := fixture.stop()
	if err != nil {
		t.Fatal("隔离同步服务关停失败", err)
	}
	verifyBackupB5ServiceStopped(t, fixture.api.baseURL, logs)
	t.Logf("B5 文件移出同步根，条目 %s 从目录 %s 移至 %s，仅拉取与确认，位置 3→4，两端正文 %d B SHA-256 %x",
		entryIDText, selectedRootID, outsideRootID, len(body), sha256.Sum256(body))
	t.Logf("本轮专用容器 MySQL %s，MinIO %s，后端已关停", fixture.mysqlContainer, fixture.minioContainer)
}

func (client b5DeletionTestClient) reconcileB5MissingFileAfterSiblingMove(t *testing.T, ctx context.Context,
	baseline dto.SyncSnapshotEntry, event dto.SyncChangeEventData, selected, sibling dto.SyncSnapshotEntry,
) bool {
	t.Helper()
	// 1. 从同一条目的移动事件读取新父目录，对照已确认基线中的同级目录。
	if event.EventType == "ENTRY_MOVED" && event.EntryID == baseline.EntryID {
		var location map[string]string
		if err := json.Unmarshal(event.Payload, &location); err != nil {
			t.Fatal("移动事件载荷无法读取", err)
		}
		if baseline.ParentID == selected.EntryID && sibling.ParentID == selected.ParentID && sibling.EntryID != selected.EntryID &&
			location["entry_id"] == baseline.EntryID && location["parent_id"] == sibling.EntryID &&
			location["revision"] == event.Revision && location["name"] == baseline.Name {
			return true
		}
	}
	// 2. 未识别为移出时，使用已有的条件传播复核。
	client.propagateLocalDeletion(t, ctx, baseline)
	return false
}
