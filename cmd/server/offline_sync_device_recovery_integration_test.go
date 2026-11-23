package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"golang.org/x/crypto/bcrypt"
)

func TestServerOfflineSyncDeviceRecoveryIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_OFFLINE_SYNC_DEVICE_RECOVERY_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_OFFLINE_SYNC_DEVICE_RECOVERY_TEST=1 后使用独占 MySQL 和回环服务")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	// 1. 建立基线，从正式拉取取得旧游标，并在保存处理结果后确认到位置 1。
	pool, databaseName := prepareServerIntegrationDatabase(t, ctx)
	password := rand.Text()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash) VALUES(1,'root','离线恢复夹具',?)", string(hash)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR')"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLOUDNEST_STORAGE_PROVIDER", "local_minio")
	prepareServerReadOnlyStorageStub(t, "fixture-offline-sync-recovery")
	address := configureBackupFixtureServer(t, databaseName)
	api, stop := startConfiguredBackupFixtureServer(t, ctx, address, run)
	login := loginBackupFixture(t, ctx, &api, password)
	userID, deviceID := backupFixtureID(t, login.User.ID), backupFixtureID(t, login.Device.ID)

	localDirectory := t.TempDir()
	pendingFile := filepath.Join(localDirectory, "pending.txt")
	const pendingContent = "离线期间生成的合成待提交内容"
	if err := os.WriteFile(pendingFile, []byte(pendingContent), 0o600); err != nil {
		t.Fatal(err)
	}
	saveLocal := func(name string, value any) {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(localDirectory, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const snapshotPath = "/api/v1/sync/snapshots"
	const pullPath = "/api/v1/sync/changes/pull"
	const confirmPath = "/api/v1/sync/changes/confirm"
	bodyWithToken := func(field, token string) *strings.Reader {
		return strings.NewReader(`{"` + field + `":` + strconv.Quote(token) + `}`)
	}
	createSnapshot := func(client backupSourceClient, key, wantPosition string) dto.SyncSnapshotPageData {
		t.Helper()
		var snapshot dto.SyncSnapshotPageResponse
		client.request(t, ctx, http.MethodPost, snapshotPath, key, strings.NewReader(`{}`), http.StatusCreated, &snapshot)
		if snapshot.RequestID == "" || snapshot.Data.SnapshotID == "" || snapshot.Data.RootEntryID != login.RootEntryID ||
			snapshot.Data.HighWaterSequence != wantPosition || snapshot.Data.BaselineToken == nil || snapshot.Data.NextCursor != nil {
			t.Fatalf("没有取得期望位置 %s 的完整快照", wantPosition)
		}
		return snapshot.Data
	}
	confirmPosition := func(client backupSourceClient, path, field, token, wantPosition string) {
		t.Helper()
		var result dto.ConfirmSyncSnapshotBaselineResponse
		client.request(t, ctx, http.MethodPost, path, "", bodyWithToken(field, token), http.StatusOK, &result)
		if result.RequestID == "" || result.Data.Status != "ACTIVE" || result.Data.AckSequence != wantPosition {
			t.Fatalf("确认结果不符：%+v，期望 %s", result, wantPosition)
		}
	}
	confirmBaseline := func(client backupSourceClient, snapshot dto.SyncSnapshotPageData) {
		t.Helper()
		saveLocal("baseline.json", map[string]any{
			"snapshot_id": snapshot.SnapshotID, "root_entry_id": snapshot.RootEntryID,
			"high_water_sequence": snapshot.HighWaterSequence, "items": snapshot.Items,
		})
		confirmPosition(client, snapshotPath+"/"+snapshot.SnapshotID+"/confirm", "baseline_token", *snapshot.BaselineToken, snapshot.HighWaterSequence)
	}
	createPullAndConfirm := func(client backupSourceClient, baselineToken, key, name, sequence string) (string, string) {
		t.Helper()
		var added dto.CreateDirectoryResponse
		client.request(t, ctx, http.MethodPost, "/api/v1/directories", key,
			strings.NewReader(`{"parent_id":`+strconv.Quote(login.RootEntryID)+`,"name":`+strconv.Quote(name)+`}`), http.StatusCreated, &added)
		var page dto.PullSyncChangesResponse
		client.request(t, ctx, http.MethodPost, pullPath, "", bodyWithToken("baseline_token", baselineToken), http.StatusOK, &page)
		if page.RequestID == "" || page.Data.NextCursor == "" || page.Data.HighWaterSequence != sequence ||
			page.Data.HasMore || len(page.Data.Events) != 1 {
			t.Fatalf("目录变更后未读到序号 %s", sequence)
		}
		event := page.Data.Events[0]
		if event.Sequence != sequence || event.EntryID != added.Data.Entry.ID || event.EventType != "DIRECTORY_CREATED" {
			t.Fatalf("增量事件不符：%s，%s，%s", event.Sequence, event.EntryID, event.EventType)
		}
		saveLocal("changes.json", page.Data)
		confirmPosition(client, confirmPath, "cursor", page.Data.NextCursor, sequence)
		return page.Data.NextCursor, added.Data.Entry.ID
	}

	first := createSnapshot(api, "offline-recovery-initial-baseline", "0")
	confirmBaseline(api, first)
	oldCursor, existingEntryID := createPullAndConfirm(api, *first.BaselineToken, "offline-recovery-initial-directory", "离线前变更", "1")
	type persistedDevice struct {
		status      domain.SyncDeviceStatus
		ackSequence uint64
		generation  string
		lastSeen    time.Time
		updated     time.Time
	}
	readDevice := func() persistedDevice {
		t.Helper()
		var state persistedDevice
		err := pool.QueryRowContext(ctx, `SELECT status,ack_sequence,HEX(cursor_generation),last_seen_at,updated_at
			FROM sync_devices WHERE user_id=? AND device_id=?`, userID, deviceID).
			Scan(&state.status, &state.ackSequence, &state.generation, &state.lastSeen, &state.updated)
		if err != nil {
			t.Fatal("读取隔离设备同步状态失败", err)
		}
		return state
	}
	active := readDevice()
	if active.status != domain.SyncDeviceStatusActive || active.ackSequence != 1 {
		t.Fatalf("首个增量确认未落库：%+v", active)
	}

	// 2. 仅回拨合成活动时间，保留密钥和会话，重启并等待正式 worker 提交。
	if _, err := stop(); err != nil {
		t.Fatal("隔离服务停止失败", err)
	}
	if result, err := pool.ExecContext(ctx, `UPDATE sync_devices SET last_seen_at=UTC_TIMESTAMP(6)-INTERVAL 91 DAY
		WHERE user_id=? AND device_id=?`, userID, deviceID); err != nil {
		t.Fatal("回拨合成同步活动失败", err)
	} else if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		t.Fatalf("活动时间回拨未命中一个设备：%d，错误 %v", affected, err)
	}
	aged := readDevice()
	restartAddress := reserveWebLoginAddress(t)
	t.Setenv("CLOUDNEST_HTTP_ADDR", restartAddress)
	restarted, stopRestarted := startConfiguredBackupFixtureServer(t, ctx, restartAddress, run)
	restarted.accessToken = api.accessToken
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	reset := readDevice()
	for reset.status != domain.SyncDeviceStatusResetRequired {
		select {
		case <-ctx.Done():
			t.Fatal("重启后后台未在期限内标记离线设备")
		case <-ticker.C:
			reset = readDevice()
		}
	}
	if reset.ackSequence != 0 || reset.generation == active.generation || reset.generation == strings.Repeat("0", 32) ||
		!reset.lastSeen.Equal(aged.lastSeen) {
		t.Fatalf("离线重建提交的设备状态不符：%+v", reset)
	}

	// 3. 旧游标在重建提交后及新基线确认后均失效，同一设备凭新游标确认位置 2。
	assertOldCursorExpired := func() {
		t.Helper()
		before := readDevice()
		for _, path := range []string{pullPath, confirmPath} {
			var failure dto.ErrorResponse
			restarted.request(t, ctx, http.MethodPost, path, "", bodyWithToken("cursor", oldCursor), http.StatusGone, &failure)
			if failure.Error.Code != "CURSOR_EXPIRED" || failure.RequestID == "" || before != readDevice() {
				t.Fatalf("旧游标失效响应或设备状态不符：%s，%+v", path, failure)
			}
		}
	}
	assertOldCursorExpired()
	next := createSnapshot(restarted, "offline-recovery-new-baseline", "1")
	var existingEntry struct {
		ID string `json:"entry_id"`
	}
	if next.SnapshotID == first.SnapshotID || len(next.Items) != 1 ||
		json.Unmarshal(next.Items[0], &existingEntry) != nil || existingEntry.ID != existingEntryID {
		t.Fatal("新快照未保存原目录变更或未生成新快照 ID")
	}
	confirmBaseline(restarted, next)
	recovered := readDevice()
	if recovered.status != domain.SyncDeviceStatusActive || recovered.ackSequence != 1 ||
		recovered.generation == active.generation || recovered.generation == reset.generation {
		t.Fatalf("同一设备未确认新的活动基线：%+v", recovered)
	}
	assertOldCursorExpired()

	newCursor, _ := createPullAndConfirm(restarted, *next.BaselineToken, "offline-recovery-next-directory", "恢复后变更", "2")
	final := readDevice()
	if newCursor == oldCursor || final.status != domain.SyncDeviceStatusActive || final.ackSequence != 2 ||
		final.generation != recovered.generation {
		t.Fatalf("恢复后的拉取与确认未推进原设备：%+v", final)
	}
	pending, err := os.ReadFile(pendingFile)
	if err != nil || string(pending) != pendingContent {
		t.Fatal("联测客户端未保留模拟待提交内容", err)
	}
	logs, err := stopRestarted()
	if err != nil || !strings.Contains(logs, "离线设备重建轮次结束") ||
		!strings.Contains(logs, `"scanned":1,"marked":1,"stale_candidates":0,"candidate_errors":0`) {
		t.Fatal("重启服务未汇报首轮离线重建或未正常退出", err)
	}
	t.Logf("独占库 %s，回环服务 %s → %s。同一设备 %d 的位置 1 经后台待重建归零，旧拉取和确认均返回 410，新基线确认到 1，再续拉和确认到 2",
		databaseName, address, restartAddress, deviceID)
}
