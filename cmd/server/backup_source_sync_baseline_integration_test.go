package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

func TestServerBackupSourceSyncBaselineIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后验证隔离源端的备份前同步基线")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	fixture := prepareBackupSource(t, ctx)
	verifyBackupSourceSyncBaseline(t, ctx, fixture)
	if _, err := fixture.stop(); err != nil {
		t.Fatal("隔离源端关停失败", err)
	}
	t.Logf("本轮专用容器 MySQL %s，MinIO %s，后端已关停", fixture.mysqlContainer, fixture.minioContainer)
}

func verifyBackupSourceSyncBaseline(t *testing.T, ctx context.Context, fixture backupSourceFixture) string {
	t.Helper()
	client, login := fixture.api, fixture.login
	artifactDir := t.TempDir()
	persist := func(name string, data, readback any) {
		t.Helper()
		content, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(artifactDir, name)
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		saved, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(saved, readback); err != nil {
			t.Fatal(err)
		}
	}
	createDirectory := func(key, name string) dto.CreateDirectoryResponse {
		t.Helper()
		var created dto.CreateDirectoryResponse
		body := strings.NewReader(fmt.Sprintf(`{"parent_id":%q,"name":%q}`, login.RootEntryID, name))
		client.request(t, ctx, http.MethodPost, "/api/v1/directories", key, body, http.StatusCreated, &created)
		return created
	}
	const snapshotPath = "/api/v1/sync/snapshots"
	const pullPath = "/api/v1/sync/changes/pull"
	const confirmPath = "/api/v1/sync/changes/confirm"

	// 1. 在隔离源端保存完整快照，确认非零基线。
	firstDirectory := createDirectory("backup-sync-initial-directory", "同步备份基线目录")
	var snapshot dto.SyncSnapshotPageResponse
	client.request(t, ctx, http.MethodPost, snapshotPath, "backup-sync-initial-snapshot", strings.NewReader(`{"page_size":1}`), http.StatusCreated, &snapshot)
	if snapshot.RequestID == "" || snapshot.Data.SnapshotID == "" || snapshot.Data.RootEntryID != login.RootEntryID ||
		snapshot.Data.HighWaterSequence != "1" || snapshot.Data.BaselineToken == nil || snapshot.Data.NextCursor != nil ||
		len(snapshot.Data.Items) != 1 {
		t.Fatal("隔离源端未交付截止序号为 1 的完整快照")
	}
	var savedSnapshot dto.SyncSnapshotPageData
	persist("snapshot.json", snapshot.Data, &savedSnapshot)
	var entry struct {
		ID string `json:"entry_id"`
	}
	if len(savedSnapshot.Items) != 1 || json.Unmarshal(savedSnapshot.Items[0], &entry) != nil || entry.ID != firstDirectory.Data.Entry.ID ||
		savedSnapshot.BaselineToken == nil || *savedSnapshot.BaselineToken == "" {
		t.Fatal("读回的快照缺少预期目录或确认凭据")
	}
	var baseline dto.ConfirmSyncSnapshotBaselineResponse
	client.request(t, ctx, http.MethodPost, snapshotPath+"/"+savedSnapshot.SnapshotID+"/confirm", "",
		strings.NewReader(fmt.Sprintf(`{"baseline_token":%q}`, *savedSnapshot.BaselineToken)), http.StatusOK, &baseline)
	if baseline.RequestID == "" || baseline.Data.Status != "ACTIVE" || baseline.Data.AckSequence != "1" {
		t.Fatal("正式接口未确认截止序号为 1 的基线")
	}

	// 2. 拉取下一条目录变更，保存并确认返回的游标。
	nextDirectory := createDirectory("backup-sync-next-directory", "停写前同步变更")
	var page dto.PullSyncChangesResponse
	client.request(t, ctx, http.MethodPost, pullPath, "",
		strings.NewReader(fmt.Sprintf(`{"baseline_token":%q,"page_size":1}`, *savedSnapshot.BaselineToken)), http.StatusOK, &page)
	if page.RequestID == "" || page.Data.HighWaterSequence != "2" || page.Data.HasMore || page.Data.NextCursor == "" ||
		len(page.Data.Events) != 1 || page.Data.Events[0].Sequence != "2" ||
		page.Data.Events[0].EntryID != nextDirectory.Data.Entry.ID || page.Data.Events[0].EventType != "DIRECTORY_CREATED" {
		t.Fatal("备份前未交付预期的第 2 条目录变更")
	}
	var savedPage dto.PullSyncChangesData
	persist("changes.json", page.Data, &savedPage)
	if savedPage.NextCursor != page.Data.NextCursor || len(savedPage.Events) != 1 || savedPage.Events[0].Sequence != "2" {
		t.Fatal("本地读回的增量页丢失已交付事件或游标")
	}
	cursorBody := fmt.Sprintf(`{"cursor":%q}`, savedPage.NextCursor)
	var confirmed dto.ConfirmSyncSnapshotBaselineResponse
	client.request(t, ctx, http.MethodPost, confirmPath, "", strings.NewReader(cursorBody), http.StatusOK, &confirmed)
	if confirmed.RequestID == "" || confirmed.Data.Status != "ACTIVE" || confirmed.Data.AckSequence != "2" {
		t.Fatal("备份前增量确认没有推进到序号 2")
	}

	// 3. 重用该游标拉取，与数据库中的确认位置交叉核对。
	var resumed dto.PullSyncChangesResponse
	client.request(t, ctx, http.MethodPost, pullPath, "", strings.NewReader(cursorBody), http.StatusOK, &resumed)
	if resumed.RequestID == "" || resumed.Data.HighWaterSequence != "2" || resumed.Data.Events == nil ||
		len(resumed.Data.Events) != 0 || resumed.Data.HasMore || resumed.Data.NextCursor == "" {
		t.Fatal("使用已保存游标在源端续读未返回预期空页")
	}
	userID := backupFixtureID(t, login.User.ID)
	deviceID := backupFixtureID(t, login.Device.ID)
	var status domain.SyncDeviceStatus
	var ack, high, retained uint64
	var generation []byte
	err := fixture.pool.QueryRowContext(ctx, `SELECT d.status,d.ack_sequence,d.cursor_generation,u.change_seq,
		(SELECT COUNT(*) FROM change_events AS events WHERE events.user_id=u.id)
		FROM sync_devices AS d JOIN users AS u ON u.id=d.user_id WHERE d.user_id=? AND d.device_id=?`, userID, deviceID).
		Scan(&status, &ack, &generation, &high, &retained)
	if err != nil || ack != 2 || high != 2 || retained != 2 || len(generation) != len(domain.SyncCursorGeneration{}) {
		t.Fatalf("备份前同步状态读回不符，确认 %d，上界 %d，保留 %d，错误 %v", ack, high, retained, err)
	}
	if state := (domain.SyncDeviceState{Status: status, AckSequence: ack, Generation: domain.SyncCursorGeneration(generation)}); !state.IsValid() ||
		status != domain.SyncDeviceStatusActive {
		t.Fatal("备份前设备未保持有效的活动代次")
	}
	t.Logf("隔离源端账号 %d，设备 %d，快照截止 1，拉取和确认到 2，同一游标续读空页，数据库 ACTIVE/ack=%d/H=%d/保留=%d，游标 SHA-256 %x",
		userID, deviceID, ack, high, retained, sha256.Sum256([]byte(savedPage.NextCursor)))
	return savedPage.NextCursor
}
