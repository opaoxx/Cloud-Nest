package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
)

type b5DeletionTestClient struct {
	api       backupSourceClient
	local     *os.Root
	propagate func(context.Context, b5DeletionCheckpoint) error
}

type b5DeletionCheckpoint struct {
	MissingLocal []dto.SyncSnapshotEntry `json:"missing_local"`
	RemoteTrash  dto.SyncChangeEventData `json:"remote_trash"`
	Cursor       string                  `json:"cursor"`
}

type b5DeletionRequestRecorder struct {
	transport http.RoundTripper
	requests  []string
}

func (recorder *b5DeletionRequestRecorder) RoundTrip(request *http.Request) (*http.Response, error) {
	recorder.requests = append(recorder.requests, request.Method+" "+request.URL.Path)
	return recorder.transport.RoundTrip(request)
}

func TestServerB5DefaultDeletionPropagationIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后验证 B5 默认关闭删除传播")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 发布两份合成文件，保存快照并确认基线。
	fixture := prepareBackupSource(t, ctx)
	userID := backupFixtureID(t, fixture.login.User.ID)
	parentID := backupFixtureID(t, fixture.login.RootEntryID)
	store, err := repository.NewStore(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	const localDeletedName = "removed-locally.txt"
	const remoteTrashedName = "trashed-remotely.txt"
	localDeletedBody := []byte("keep this content on the server\n")
	remoteTrashedBody := []byte("keep this content on the client\n")
	localDeletedID, remoteObject := publishBackupSourceFile(t, ctx, fixture.api, store, fixture.bucket,
		userID, parentID, 1, localDeletedName, localDeletedBody)
	remoteTrashedID, _ := publishBackupSourceFile(t, ctx, fixture.api, store, fixture.bucket,
		userID, parentID, 2, remoteTrashedName, remoteTrashedBody)
	local, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := local.Close(); err != nil {
			t.Error("关闭本地合成目录失败", err)
		}
	})
	for name, body := range map[string][]byte{localDeletedName: localDeletedBody, remoteTrashedName: remoteTrashedBody} {
		if err := local.WriteFile(name, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := establishB5DeletionTestBaseline(t, ctx, fixture.api, local, fixture.login.RootEntryID)
	before := readBackupB5DatabaseState(t, ctx, fixture.pool, userID)
	if before.high != 2 || len(before.events) != 2 || len(before.devices) != 1 ||
		before.devices[0].DeviceID != backupFixtureID(t, fixture.login.Device.ID) ||
		before.devices[0].State.Status != domain.SyncDeviceStatusActive || before.devices[0].State.AckSequence != 2 {
		t.Fatal("合成设备未确认两份文件所在的基线")
	}

	// 2. 制造一次本地缺项和一次远端回收，记录两种变化。
	if err := local.Remove(localDeletedName); err != nil {
		t.Fatal("未能移除本轮临时目录的合成文件", err)
	}
	otherDevice := fixture.api
	otherLogin := loginBackupFixture(t, ctx, &otherDevice, os.Getenv("CLOUDNEST_ADMIN_PASSWORD"))
	if otherLogin.User.ID != fixture.login.User.ID || otherLogin.Device.ID == fixture.login.Device.ID {
		t.Fatal("远端回收动作需要同一合成账号的另一设备")
	}
	var trashed dto.TrashEntryResponse
	otherDevice.request(t, ctx, http.MethodPost, fmt.Sprintf("/api/v1/entries/%d/trash", remoteTrashedID), "b5-other-device-trashes-one-file",
		strings.NewReader(`{"expected_revision":"1"}`), http.StatusOK, &trashed)
	if trashed.RequestID == "" || trashed.Data.Entry.Status != "TRASHED" || trashed.Data.Entry.Revision != "2" ||
		trashed.Data.Entry.ID != strconv.FormatUint(remoteTrashedID, 10) {
		t.Fatal("另一设备未按预期制造正式的文件回收事件")
	}
	recorder := &b5DeletionRequestRecorder{transport: http.DefaultTransport}
	syncAPI := fixture.api
	syncAPI.httpClient = &http.Client{
		Transport: recorder, Timeout: fixture.api.httpClient.Timeout, CheckRedirect: fixture.api.httpClient.CheckRedirect,
	}
	client := b5DeletionTestClient{api: syncAPI, local: local}
	checkpoint := client.saveObservations(t, ctx, snapshot)
	if len(checkpoint.MissingLocal) != 1 || checkpoint.MissingLocal[0].EntryID != strconv.FormatUint(localDeletedID, 10) ||
		checkpoint.MissingLocal[0].Name != localDeletedName || checkpoint.MissingLocal[0].Revision != "1" ||
		checkpoint.RemoteTrash.Sequence != "3" || checkpoint.RemoteTrash.EntryID != trashed.Data.Entry.ID ||
		checkpoint.RemoteTrash.Revision != trashed.Data.Entry.Revision {
		t.Fatal("本地保存的检查点必须包含准确的缺项和远端墓碑")
	}
	wantTrashPayload := map[string]string{
		"entry_id": trashed.Data.Entry.ID, "parent_id": fixture.login.RootEntryID,
		"name": remoteTrashedName, "revision": trashed.Data.Entry.Revision,
	}
	var trashPayload map[string]string
	if err := json.Unmarshal(checkpoint.RemoteTrash.Payload, &trashPayload); err != nil || !reflect.DeepEqual(trashPayload, wantTrashPayload) {
		t.Fatal("读回的远端墓碑未保留原位置、名称及回收后修订", err)
	}
	unconfirmed := readBackupB5DatabaseState(t, ctx, fixture.pool, userID)
	if unconfirmed.high != 3 || !reflect.DeepEqual(unconfirmed.devices, before.devices) ||
		len(unconfirmed.events) != 3 || !reflect.DeepEqual(unconfirmed.events[:2], before.events) ||
		unconfirmed.events[2].eventType != "ENTRY_TRASHED" || unconfirmed.events[2].entryID != remoteTrashedID ||
		unconfirmed.used != before.used || unconfirmed.reserved != 0 {
		t.Fatal("拉取墓碑后只允许测试设备的原始回收事件，确认位置仍应为 2")
	}

	// 3. 保存墓碑和游标后确认，只核对同步进度与两侧保留的内容。
	var confirmed dto.ConfirmSyncSnapshotBaselineResponse
	syncAPI.request(t, ctx, http.MethodPost, "/api/v1/sync/changes/confirm", "",
		strings.NewReader(fmt.Sprintf(`{"cursor":%q}`, checkpoint.Cursor)), http.StatusOK, &confirmed)
	if confirmed.RequestID == "" || confirmed.Data.Status != "ACTIVE" || confirmed.Data.AckSequence != "3" {
		t.Fatal("原设备未确认读回的墓碑游标至序号 3")
	}
	if !reflect.DeepEqual(recorder.requests, []string{"POST /api/v1/sync/changes/pull", "POST /api/v1/sync/changes/confirm"}) {
		t.Fatal("默认客户端发出了意外的请求", recorder.requests)
	}
	unconfirmed.devices[0].State.AckSequence = 3
	if after := readBackupB5DatabaseState(t, ctx, fixture.pool, userID); !reflect.DeepEqual(after, unconfirmed) {
		t.Fatal("确认墓碑后应仅推进确认位置")
	}
	if retained, err := local.ReadFile(remoteTrashedName); err != nil || !bytes.Equal(retained, remoteTrashedBody) {
		t.Fatal("远端墓碑导致本地合成正文丢失或改变", err)
	}
	if _, err := local.Stat(localDeletedName); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("本地已移除样本与实际扫描状态不符", err)
	}
	verifyReplacementDownload(t, ctx, fixture, fmt.Sprintf("/api/v1/entries/%d", localDeletedID), 1, remoteObject, localDeletedBody)
	logs, err := fixture.stop()
	if err != nil {
		t.Fatal("隔离同步服务关停失败", err)
	}
	verifyBackupB5ServiceStopped(t, fixture.api.baseURL, logs)
	t.Logf("B5 默认关闭删除传播，本地缺项 %d 保留远端正文 SHA-256 %x，远端墓碑 %d 保留本地正文 SHA-256 %x，只拉取和确认，位置 2→3",
		localDeletedID, sha256.Sum256(localDeletedBody), remoteTrashedID, sha256.Sum256(remoteTrashedBody))
	t.Logf("本轮专用容器 MySQL %s，MinIO %s，后端已关停", fixture.mysqlContainer, fixture.minioContainer)
}

func establishB5DeletionTestBaseline(t *testing.T, ctx context.Context, api backupSourceClient, local *os.Root, rootID string) dto.SyncSnapshotPageData {
	t.Helper()
	// 1. 读回正式快照并核对两份本地合成文件，再确认末页。
	const path = "/api/v1/sync/snapshots"
	var snapshot dto.SyncSnapshotPageResponse
	api.request(t, ctx, http.MethodPost, path, "b5-deletion-default-baseline", strings.NewReader("{}"), http.StatusCreated, &snapshot)
	if snapshot.RequestID == "" || snapshot.Data.SnapshotID == "" || snapshot.Data.RootEntryID != rootID ||
		snapshot.Data.HighWaterSequence != "2" || snapshot.Data.NextCursor != nil ||
		snapshot.Data.BaselineToken == nil || *snapshot.Data.BaselineToken == "" || len(snapshot.Data.Items) != 2 {
		t.Fatal("合成文件未进入同一完整快照")
	}
	var saved dto.SyncSnapshotPageData
	saveAndReadB5DeletionFixtureJSON(t, local, "baseline.json", snapshot.Data, &saved)
	if !reflect.DeepEqual(saved, snapshot.Data) {
		t.Fatal("保存的基线与正式接口交付不符")
	}
	for _, raw := range saved.Items {
		var entry dto.SyncSnapshotEntry
		if err := json.Unmarshal(raw, &entry); err != nil || entry.EntryType != "FILE" || entry.ParentID != rootID || entry.SHA256 == nil {
			t.Fatal("基线条目缺少合成文件的定位或摘要", err)
		}
		body, err := local.ReadFile(entry.Name)
		if err != nil || uint64(len(body)) != entry.SizeBytes || fmt.Sprintf("%x", sha256.Sum256(body)) != *entry.SHA256 {
			t.Fatal("已确认基线前两侧应具有相同的合成文件", entry.Name, err)
		}
	}
	var confirmed dto.ConfirmSyncSnapshotBaselineResponse
	api.request(t, ctx, http.MethodPost, path+"/"+saved.SnapshotID+"/confirm", "",
		strings.NewReader(fmt.Sprintf(`{"baseline_token":%q}`, *saved.BaselineToken)), http.StatusOK, &confirmed)
	if confirmed.RequestID == "" || confirmed.Data.Status != "ACTIVE" || confirmed.Data.AckSequence != "2" {
		t.Fatal("原设备未确认已保存的基线")
	}
	return saved
}

func (client b5DeletionTestClient) saveObservations(t *testing.T, ctx context.Context, snapshot dto.SyncSnapshotPageData) b5DeletionCheckpoint {
	t.Helper()
	// 1. 比对基线与本地文件，记录已缺失的条目。
	var checkpoint b5DeletionCheckpoint
	for _, raw := range snapshot.Items {
		var entry dto.SyncSnapshotEntry
		if err := json.Unmarshal(raw, &entry); err != nil {
			t.Fatal(err)
		}
		if _, err := client.local.Stat(entry.Name); errors.Is(err, os.ErrNotExist) {
			checkpoint.MissingLocal = append(checkpoint.MissingLocal, entry)
		} else if err != nil {
			t.Fatal("检查本地合成文件失败", err)
		}
	}

	// 2. 拉取真实回收事件。测试客户端默认未配置传播动作，仍然保存事件。
	var page dto.PullSyncChangesResponse
	client.api.request(t, ctx, http.MethodPost, "/api/v1/sync/changes/pull", "",
		strings.NewReader(fmt.Sprintf(`{"baseline_token":%q}`, *snapshot.BaselineToken)), http.StatusOK, &page)
	if page.RequestID == "" || page.Data.HasMore || page.Data.HighWaterSequence != "3" || page.Data.NextCursor == "" ||
		len(page.Data.Events) != 1 || page.Data.Events[0].EventType != "ENTRY_TRASHED" {
		t.Fatal("增量页未交付唯一的正式回收事件")
	}
	checkpoint.RemoteTrash, checkpoint.Cursor = page.Data.Events[0], page.Data.NextCursor
	if client.propagate != nil {
		if err := client.propagate(ctx, checkpoint); err != nil {
			t.Fatal("删除传播动作失败", err)
		}
	}
	var saved b5DeletionCheckpoint
	saveAndReadB5DeletionFixtureJSON(t, client.local, "deletion-checkpoint.json", checkpoint, &saved)
	if !reflect.DeepEqual(saved, checkpoint) {
		t.Fatal("本地检查点未保存完整墓碑和游标")
	}
	return saved
}

func saveAndReadB5DeletionFixtureJSON(t *testing.T, local *os.Root, name string, value, readback any) {
	t.Helper()
	content, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.WriteFile(name, content, 0o600); err != nil {
		t.Fatal(err)
	}
	saved, err := local.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(saved, readback); err != nil {
		t.Fatal(err)
	}
}
