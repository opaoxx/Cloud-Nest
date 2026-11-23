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

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
)

type b5SnapshotRebuildCheckpoint struct {
	Baseline     dto.SyncSnapshotPageData `json:"baseline"`
	PendingLocal []dto.SyncSnapshotEntry  `json:"pending_local"`
}

func TestServerB5SnapshotRebuildRetainsLocalIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后验证 B5 快照重建保留本地文件")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	const snapshotPath = "/api/v1/sync/snapshots"
	const pullPath = "/api/v1/sync/changes/pull"

	// 1. 保存并确认三份文件的旧基线，取得旧增量游标。
	fixture := prepareBackupSource(t, ctx)
	userID := backupFixtureID(t, fixture.login.User.ID)
	deviceID := backupFixtureID(t, fixture.login.Device.ID)
	rootID := backupFixtureID(t, fixture.login.RootEntryID)
	store, err := repository.NewStore(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	local, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := local.Close(); err != nil {
			t.Error("关闭本地合成目录失败", err)
		}
	})
	samples := []struct {
		name    string
		body    []byte
		entryID uint64
		object  *domain.StoredObject
	}{
		{name: "edited.txt", body: []byte("original local copy\n")},
		{name: "retained.txt", body: []byte("unchanged local copy\n")},
		{name: "active.txt", body: []byte("active on both sides\n")},
	}
	var used uint64
	for index := range samples {
		sample := &samples[index]
		sample.entryID, sample.object = publishBackupSourceFile(t, ctx, fixture.api, store, fixture.bucket,
			userID, rootID, index+1, sample.name, sample.body)
		if err := local.WriteFile(sample.name, sample.body, 0o600); err != nil {
			t.Fatal(err)
		}
		used += uint64(len(sample.body))
	}
	oldBaseline := establishB5DeletionTestBaseline(t, ctx, fixture.api, local, fixture.login.RootEntryID, len(samples))
	var oldPull dto.PullSyncChangesResponse
	fixture.api.request(t, ctx, http.MethodPost, pullPath, "",
		strings.NewReader(fmt.Sprintf(`{"baseline_token":%q}`, *oldBaseline.BaselineToken)), http.StatusOK, &oldPull)
	if oldPull.RequestID == "" || oldPull.Data.NextCursor == "" || oldPull.Data.HasMore ||
		oldPull.Data.HighWaterSequence != "3" || len(oldPull.Data.Events) != 0 {
		t.Fatal("旧基线后应取得序号 3 的可续读游标")
	}
	var oldCheckpoint dto.PullSyncChangesData
	saveAndReadB5DeletionFixtureJSON(t, local, "old-incremental.json", oldPull.Data, &oldCheckpoint)
	if !reflect.DeepEqual(oldCheckpoint, oldPull.Data) {
		t.Fatal("旧增量位置未正确保存")
	}
	activeState := readBackupB5DatabaseState(t, ctx, fixture.pool, userID)
	if len(activeState.devices) != 1 || activeState.devices[0].DeviceID != deviceID ||
		activeState.devices[0].State.Status != domain.SyncDeviceStatusActive || activeState.devices[0].State.AckSequence != 3 {
		t.Fatal("原设备应已确认位置 3")
	}

	// 2. 制造两条远端缺项，触发离线重建并验证旧游标失效。
	edited := []byte("local edit awaiting reconciliation\n")
	if err := local.WriteFile(samples[0].name, edited, 0o600); err != nil {
		t.Fatal("无法模拟待提交的本地编辑", err)
	}
	otherAPI := fixture.api
	otherLogin := loginBackupFixture(t, ctx, &otherAPI, os.Getenv("CLOUDNEST_ADMIN_PASSWORD"))
	if otherLogin.User.ID != fixture.login.User.ID || otherLogin.Device.ID == "" || otherLogin.Device.ID == fixture.login.Device.ID {
		t.Fatal("缺项应由同一合成账号的另一设备制造")
	}
	for _, sample := range samples[:2] {
		var trashed dto.TrashEntryResponse
		otherAPI.request(t, ctx, http.MethodPost, fmt.Sprintf("/api/v1/entries/%d/trash", sample.entryID),
			"b5-rebuild-missing-"+sample.name, strings.NewReader(`{"expected_revision":"1"}`), http.StatusOK, &trashed)
		if trashed.RequestID == "" || trashed.Data.Entry.ID != fmt.Sprint(sample.entryID) ||
			trashed.Data.Entry.Status != "TRASHED" || trashed.Data.Entry.Revision != "2" {
			t.Fatal("远端回收夹具未提交", sample.name)
		}
	}
	businessBefore := readBackupB5DatabaseState(t, ctx, fixture.pool, userID)
	if businessBefore.high != 5 || businessBefore.used != used || businessBefore.reserved != 0 ||
		len(businessBefore.events) != 5 || !reflect.DeepEqual(businessBefore.events[:3], activeState.events) ||
		businessBefore.events[3].eventType != "ENTRY_TRASHED" || businessBefore.events[3].entryID != samples[0].entryID ||
		businessBefore.events[4].eventType != "ENTRY_TRASHED" || businessBefore.events[4].entryID != samples[1].entryID {
		t.Fatal("快照重建前应只有另一设备提交的两条回收事件")
	}
	logs, err := fixture.stop()
	if err != nil {
		t.Fatal("初始隔离服务关停失败", err)
	}
	verifyBackupB5ServiceStopped(t, fixture.api.baseURL, logs)
	result, err := fixture.pool.ExecContext(ctx, `UPDATE sync_devices SET last_seen_at=UTC_TIMESTAMP(6)-INTERVAL 91 DAY
		WHERE user_id=? AND device_id=?`, userID, deviceID)
	if err != nil {
		t.Fatal("回拨本轮合成设备活动时间失败", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		t.Fatal("时间回拨应只命中原合成设备", affected, err)
	}
	address := reserveWebLoginAddress(t)
	t.Setenv("CLOUDNEST_HTTP_ADDR", address)
	restarted, stop := startConfiguredBackupFixtureServer(t, ctx, address, run)
	restarted.accessToken = fixture.api.accessToken
	reset := awaitB5SnapshotRebuildReset(t, ctx, store, userID, deviceID)
	if reset.AckSequence != 0 || reset.Generation == activeState.devices[0].State.Generation {
		t.Fatal("离线重建应归零旧设备位置并更换代次")
	}
	recorder := &b5DeletionRequestRecorder{transport: http.DefaultTransport}
	recoveryAPI := restarted
	recoveryAPI.httpClient = &http.Client{
		Transport: recorder, Timeout: restarted.httpClient.Timeout, CheckRedirect: restarted.httpClient.CheckRedirect,
	}
	var expired dto.ErrorResponse
	recoveryAPI.request(t, ctx, http.MethodPost, pullPath, "",
		strings.NewReader(fmt.Sprintf(`{"cursor":%q}`, oldCheckpoint.NextCursor)), http.StatusGone, &expired)
	if expired.RequestID == "" || expired.Error.Code != "CURSOR_EXPIRED" {
		t.Fatal("旧游标未按契约失效")
	}

	// 3. 保存完整新快照，保留缺项对应的本地正文，再确认新基线。
	var refreshed dto.SyncSnapshotPageResponse
	recoveryAPI.request(t, ctx, http.MethodPost, snapshotPath, "b5-rebuild-new-baseline",
		strings.NewReader("{}"), http.StatusCreated, &refreshed)
	next := refreshed.Data
	if refreshed.RequestID == "" || next.SnapshotID == "" || next.SnapshotID == oldBaseline.SnapshotID ||
		next.RootEntryID != oldBaseline.RootEntryID || next.HighWaterSequence != "5" ||
		next.BaselineToken == nil || *next.BaselineToken == "" || next.NextCursor != nil || len(next.Items) != 1 {
		t.Fatal("重建应交付完整的新快照，只剩一份活动文件")
	}
	var survivor dto.SyncSnapshotEntry
	if err := json.Unmarshal(next.Items[0], &survivor); err != nil || survivor.EntryID != fmt.Sprint(samples[2].entryID) ||
		survivor.ParentID != fixture.login.RootEntryID || survivor.Name != samples[2].name || survivor.Revision != "1" {
		t.Fatal("新基线应保留正确的活动文件", err)
	}
	checkpoint := saveB5SnapshotRebuildCheckpoint(t, local, oldBaseline, next)
	if len(checkpoint.PendingLocal) != 2 || checkpoint.PendingLocal[0].EntryID != fmt.Sprint(samples[0].entryID) ||
		checkpoint.PendingLocal[0].Name != samples[0].name || checkpoint.PendingLocal[1].EntryID != fmt.Sprint(samples[1].entryID) ||
		checkpoint.PendingLocal[1].Name != samples[1].name {
		t.Fatal("两条缺项应记录为待核对的本地路径")
	}
	confirmPath := snapshotPath + "/" + checkpoint.Baseline.SnapshotID + "/confirm"
	var confirmed dto.ConfirmSyncSnapshotBaselineResponse
	recoveryAPI.request(t, ctx, http.MethodPost, confirmPath, "",
		strings.NewReader(fmt.Sprintf(`{"baseline_token":%q}`, *checkpoint.Baseline.BaselineToken)), http.StatusOK, &confirmed)
	if confirmed.RequestID == "" || confirmed.Data.Status != "ACTIVE" || confirmed.Data.AckSequence != "5" ||
		!reflect.DeepEqual(recorder.requests, []string{"POST " + pullPath, "POST " + snapshotPath, "POST " + confirmPath}) {
		t.Fatal("重建应只使用旧游标一次，随后创建并确认新快照", recorder.requests)
	}
	for index, expected := range [][]byte{edited, samples[1].body, samples[2].body} {
		if retained, err := local.ReadFile(samples[index].name); err != nil || !bytes.Equal(retained, expected) {
			t.Fatal("完整新快照不能导致丢失旧路径正文", samples[index].name, err)
		}
	}
	after := readBackupB5DatabaseState(t, ctx, fixture.pool, userID)
	if len(after.devices) != 1 || after.devices[0].DeviceID != deviceID ||
		after.devices[0].State.Status != domain.SyncDeviceStatusActive || after.devices[0].State.AckSequence != 5 ||
		after.devices[0].State.Generation == reset.Generation || after.devices[0].State.Generation == activeState.devices[0].State.Generation {
		t.Fatal("同一设备应以新代次确认到 5")
	}
	businessBefore.devices = after.devices
	if !reflect.DeepEqual(after, businessBefore) {
		t.Fatal("重建不应产生新的业务事件或改变配额")
	}
	downloadFixture := fixture
	downloadFixture.api = restarted
	verifyReplacementDownload(t, ctx, downloadFixture, fmt.Sprintf("/api/v1/entries/%d", samples[2].entryID),
		1, samples[2].object, samples[2].body)
	logs, err = stop()
	if err != nil {
		t.Fatal("重启后的隔离服务关停失败", err)
	}
	verifyBackupB5ServiceStopped(t, restarted.baseURL, logs)
	t.Logf("B5 快照重建缺项，旧游标返回 410，三项变一项，保留两条旧路径，确认位置 5，已用 %d B，本地编辑与原副本 SHA-256 %x、%x",
		used, sha256.Sum256(edited), sha256.Sum256(samples[1].body))
	t.Logf("本轮专用容器 MySQL %s，MinIO %s，后端已关停", fixture.mysqlContainer, fixture.minioContainer)
}

func awaitB5SnapshotRebuildReset(t *testing.T, ctx context.Context, store *repository.Store, userID, deviceID uint64) domain.SyncDeviceState {
	t.Helper()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	waitContext, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	// 1. 等正式离线重建任务提交原设备的待重建状态。
	for {
		devices, err := store.ListRestoredSyncDeviceCandidates(waitContext, 0, 2)
		if err != nil || len(devices) != 1 || devices[0].UserID != userID || devices[0].DeviceID != deviceID {
			t.Fatal("无法读取本轮合成设备状态", err)
		}
		if devices[0].State.Status == domain.SyncDeviceStatusResetRequired {
			return devices[0].State
		}
		select {
		case <-waitContext.Done():
			t.Fatal("隔离服务未在期限内标记离线设备", waitContext.Err())
		case <-ticker.C:
		}
	}
}

func saveB5SnapshotRebuildCheckpoint(t *testing.T, local *os.Root, oldBaseline, next dto.SyncSnapshotPageData) b5SnapshotRebuildCheckpoint {
	t.Helper()
	readEntry := func(raw json.RawMessage) dto.SyncSnapshotEntry {
		t.Helper()
		var entry dto.SyncSnapshotEntry
		if err := json.Unmarshal(raw, &entry); err != nil || entry.EntryID == "" {
			t.Fatal("无法读懂快照条目", err)
		}
		return entry
	}
	// 1. 只将新快照当前交付的条目纳入远端基线。
	activeIDs := make(map[string]bool, len(next.Items))
	for _, raw := range next.Items {
		activeIDs[readEntry(raw).EntryID] = true
	}
	checkpoint := b5SnapshotRebuildCheckpoint{Baseline: next}
	// 2. 旧快照缺项记为待核对，保留本地文件，再持久化整份新基线。
	for _, raw := range oldBaseline.Items {
		entry := readEntry(raw)
		if !activeIDs[entry.EntryID] {
			checkpoint.PendingLocal = append(checkpoint.PendingLocal, entry)
		}
	}
	var saved b5SnapshotRebuildCheckpoint
	saveAndReadB5DeletionFixtureJSON(t, local, "rebuilt-baseline.json", checkpoint, &saved)
	if !reflect.DeepEqual(saved, checkpoint) {
		t.Fatal("新基线与待核对路径未一起保存")
	}
	return saved
}
