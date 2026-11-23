package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestServerBackupTargetB5CursorRestartIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后复验恢复目标新游标跨重启")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 恢复目标并确认新基线，保存序号 4 的增量游标。
	source := prepareBackupSource(t, ctx)
	signingSecret := os.Getenv("CLOUDNEST_JWT_SECRET")
	backupDir := t.TempDir()
	manifest := prepareBackupSourceB5Manifest(t, ctx, source, backupDir)
	userID := backupFixtureID(t, source.login.User.ID)
	deviceID := backupFixtureID(t, source.login.Device.ID)
	backupState := readBackupB5DatabaseState(t, ctx, source.pool, userID)
	fixture := restoreBackupTargetB5Remap(t, ctx, source, backupDir, manifest, backupState)
	verifyAndApplyBackupTargetB5ObjectMap(t, ctx, fixture)
	resetBackupTargetB5SyncDevices(t, ctx, fixture)
	target, stopTarget := startBackupTargetB5WithSourceSession(t, ctx, fixture, signingSecret)

	const pullPath = "/api/v1/sync/changes/pull"
	baseline := confirmBackupTargetB5Baseline(t, ctx, target, source.login.RootEntryID, "4")
	before := readBackupB5DatabaseState(t, ctx, fixture.database.pool, userID)
	if len(before.devices) != 1 || before.devices[0].UserID != userID || before.devices[0].DeviceID != deviceID ||
		before.devices[0].State.Status != domain.SyncDeviceStatusActive || before.devices[0].State.AckSequence != 4 ||
		before.devices[0].State.Generation == backupState.devices[0].State.Generation || before.high != 4 {
		t.Fatal("恢复目标未在新代次上确认到备份截止位置 4")
	}
	activityBefore := readBackupB5SyncDeviceTimes(t, ctx, fixture.database.pool, userID, deviceID)
	var initial dto.PullSyncChangesResponse
	target.request(t, ctx, http.MethodPost, pullPath, "", strings.NewReader(fmt.Sprintf(`{"baseline_token":%q}`, baseline)), http.StatusOK, &initial)
	if initial.RequestID == "" || initial.Data.HighWaterSequence != "4" || initial.Data.NextCursor == "" ||
		initial.Data.Events == nil || len(initial.Data.Events) != 0 || initial.Data.HasMore {
		t.Fatal("已确认的新基线应交付位置 4 的空页和游标")
	}
	clientDir := t.TempDir()
	checkpointPath := filepath.Join(clientDir, "checkpoint-4.json")
	saveBackupTargetB5ChangePage(t, checkpointPath, initial.Data)

	// 2. 普通重启目标，核对同步代次和确认位置。
	logs, err := stopTarget()
	if err != nil {
		t.Fatal("隔离目标首次关停失败", err)
	}
	verifyBackupSourceStopped(t, target.baseURL, logs)
	restarted, stopRestarted := startBackupTargetB5WithSourceSession(t, ctx, fixture, signingSecret)
	if afterRestart := readBackupB5DatabaseState(t, ctx, fixture.database.pool, userID); !reflect.DeepEqual(afterRestart, before) ||
		readBackupB5SyncDeviceTimes(t, ctx, fixture.database.pool, userID, deviceID) != activityBefore {
		t.Fatal("普通重启改变了设备代次、确认位置、活动时间或备份日志和配额")
	}

	// 3. 从保存的游标续读序号 5，保存事件后确认并读回数据库。
	const directoryName = "恢复目标重启后目录"
	var created dto.CreateDirectoryResponse
	restarted.request(t, ctx, http.MethodPost, "/api/v1/directories", "b5-target-after-restart-directory",
		strings.NewReader(fmt.Sprintf(`{"parent_id":%q,"name":%q}`, source.login.RootEntryID, directoryName)), http.StatusCreated, &created)
	checkpoint := readBackupTargetB5ChangePage(t, checkpointPath)
	if !reflect.DeepEqual(checkpoint, initial.Data) {
		t.Fatal("重启后读回的客户端检查点与原交付不符")
	}
	signingConfig, err := config.LoadJWTConfig("")
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := service.NewSyncChangeCursorVerifier(signingConfig.SigningKey)
	if err != nil {
		t.Fatal(err)
	}
	generation := before.devices[0].State.Generation
	verifyBackupTargetB5RestartCursor(t, verifier, userID, deviceID, generation, 4, checkpoint.NextCursor)

	var page dto.PullSyncChangesResponse
	restarted.request(t, ctx, http.MethodPost, pullPath, "", strings.NewReader(fmt.Sprintf(`{"cursor":%q}`, checkpoint.NextCursor)), http.StatusOK, &page)
	if page.RequestID == "" || page.Data.HighWaterSequence != "5" || page.Data.HasMore || len(page.Data.Events) != 1 ||
		page.Data.Events[0].Sequence != "5" || page.Data.Events[0].EntryID != created.Data.Entry.ID ||
		page.Data.Events[0].Revision != created.Data.Entry.Revision || page.Data.Events[0].EventType != "DIRECTORY_CREATED" {
		t.Fatal("重启后未从检查点续读到唯一的新目录事件")
	}
	pulledState := readBackupB5DatabaseState(t, ctx, fixture.database.pool, userID)
	if pulledState.high != 5 || pulledState.used != before.used || pulledState.reserved != before.reserved ||
		!reflect.DeepEqual(pulledState.devices, before.devices) || len(pulledState.events) != 5 ||
		!reflect.DeepEqual(pulledState.events[:4], before.events) ||
		readBackupB5SyncDeviceTimes(t, ctx, fixture.database.pool, userID, deviceID) != activityBefore {
		t.Fatal("拉取后应仍确认到 4，数据库日志只追加一条事件")
	}
	delivered := page.Data.Events[0]
	stored := pulledState.events[4]
	var compactPayload bytes.Buffer
	if err := json.Compact(&compactPayload, []byte(stored.payload)); err != nil ||
		stored.sequence != 5 || fmt.Sprint(stored.entryID) != delivered.EntryID || fmt.Sprint(stored.revision) != delivered.Revision ||
		stored.eventType != delivered.EventType || !bytes.Equal(compactPayload.Bytes(), delivered.Payload) {
		t.Fatal("事件 5 的数据库记录与正式接口交付不一致", err)
	}
	var payload struct {
		Name     string `json:"name"`
		ParentID string `json:"parent_id"`
	}
	if err := json.Unmarshal(delivered.Payload, &payload); err != nil ||
		payload.Name != directoryName || payload.ParentID != source.login.RootEntryID {
		t.Fatal("增量载荷的目录名称或位置不符", err)
	}

	nextPath := filepath.Join(clientDir, "checkpoint-5.json")
	saveBackupTargetB5ChangePage(t, nextPath, page.Data)
	persisted := readBackupTargetB5ChangePage(t, nextPath)
	if !reflect.DeepEqual(persisted, page.Data) {
		t.Fatal("确认前读回的客户端事件和游标与交付不符")
	}
	verifyBackupTargetB5RestartCursor(t, verifier, userID, deviceID, generation, 5, persisted.NextCursor)
	var confirmed dto.ConfirmSyncSnapshotBaselineResponse
	restarted.request(t, ctx, http.MethodPost, "/api/v1/sync/changes/confirm", "",
		strings.NewReader(fmt.Sprintf(`{"cursor":%q}`, persisted.NextCursor)), http.StatusOK, &confirmed)
	if confirmed.RequestID == "" || confirmed.Data.Status != "ACTIVE" || confirmed.Data.AckSequence != "5" {
		t.Fatal("隔离目标未确认已保存的序号 5 游标")
	}
	pulledState.devices[0].State.AckSequence = 5
	if after := readBackupB5DatabaseState(t, ctx, fixture.database.pool, userID); !reflect.DeepEqual(after, pulledState) {
		t.Fatal("确认后的持久代次、位置、日志或配额不符")
	}
	logs, err = stopRestarted()
	if err != nil {
		t.Fatal("重启后的隔离目标关停失败", err)
	}
	verifyBackupSourceStopped(t, restarted.baseURL, logs)
	t.Logf("B5 恢复目标普通重启 %s → %s，设备 %d 保留新代次，位置 4 检查点 SHA-256 %x，续读唯一事件 5 后确认到 5，已用 %d B，预占 %d B",
		target.baseURL, restarted.baseURL, deviceID, sha256.Sum256([]byte(checkpoint.NextCursor)), before.used, before.reserved)
	t.Logf("本轮专用容器 MySQL %s，MinIO %s，后端已关停", source.mysqlContainer, source.minioContainer)
}

func verifyBackupTargetB5RestartCursor(t *testing.T, verifier *service.SyncChangeCursorVerifier,
	userID, deviceID uint64, generation domain.SyncCursorGeneration, sequence uint64, cursor string,
) {
	t.Helper()
	if position, err := verifier.Verify(userID, deviceID, cursor); err != nil || position == nil ||
		position.Generation != generation || position.Sequence != sequence {
		t.Fatalf("客户端检查点未绑定新代次及序号 %d，错误 %v", sequence, err)
	}
}

func saveBackupTargetB5ChangePage(t *testing.T, path string, page dto.PullSyncChangesData) {
	t.Helper()
	encoded, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readBackupTargetB5ChangePage(t *testing.T, path string) dto.PullSyncChangesData {
	t.Helper()
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var page dto.PullSyncChangesData
	if err := json.Unmarshal(saved, &page); err != nil {
		t.Fatal(err)
	}
	return page
}
