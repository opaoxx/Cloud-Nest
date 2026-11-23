package main

import (
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

func TestServerBackupTargetB5OldCursorIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后复验恢复目标拒绝旧代次游标")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 保留停写状态，在源端取得序号 5 的旧代次游标。
	source := prepareBackupSource(t, ctx)
	signingSecret := os.Getenv("CLOUDNEST_JWT_SECRET")
	backupDir := t.TempDir()
	manifest, originalCursor := prepareBackupSourceB5ManifestAndCursor(t, ctx, source, backupDir)
	userID := backupFixtureID(t, source.login.User.ID)
	deviceID := backupFixtureID(t, source.login.Device.ID)
	backupState := readBackupB5DatabaseState(t, ctx, source.pool, userID)
	oldCursor := createBackupSourceB5PostBackupCursor(t, ctx, source, backupDir, manifest, originalCursor)
	sourceAfter := readBackupB5DatabaseState(t, ctx, source.pool, userID)

	// 2. 恢复并换代目标，用正式接口写入内容不同的序号 5。
	fixture := restoreBackupTargetB5Remap(t, ctx, source, backupDir, manifest, backupState)
	verifyAndApplyBackupTargetB5ObjectMap(t, ctx, fixture)
	resetBackupTargetB5SyncDevices(t, ctx, fixture)
	target, stopTarget := startBackupTargetB5WithSourceSession(t, ctx, fixture, signingSecret)
	var profile dto.UserProfileResponse
	target.request(t, ctx, http.MethodGet, "/api/v1/me", "", nil, http.StatusOK, &profile)
	if profile.RequestID == "" || profile.Data.User.ID != source.login.User.ID || profile.Data.RootEntryID != source.login.RootEntryID {
		t.Fatal("目标未接受恢复出的同一合成会话")
	}
	const targetName = "目标复用序号的新目录"
	var created dto.CreateDirectoryResponse
	target.request(t, ctx, http.MethodPost, "/api/v1/directories", "b5-target-reused-sequence-directory",
		strings.NewReader(fmt.Sprintf(`{"parent_id":%q,"name":%q}`, profile.Data.RootEntryID, targetName)), http.StatusCreated, &created)
	pending := readBackupB5DatabaseState(t, ctx, fixture.database.pool, userID)
	if pending.high != 5 || len(pending.events) != 5 || pending.events[4].sequence != 5 ||
		pending.events[4].eventType != "DIRECTORY_CREATED" || fmt.Sprint(pending.events[4].entryID) != created.Data.Entry.ID ||
		!reflect.DeepEqual(pending.events[:4], backupState.events) || pending.events[4].payload == sourceAfter.events[4].payload {
		t.Fatal("目标未复用序号 5 写入一条与源端不同的新事件")
	}
	var eventPayload struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(pending.events[4].payload), &eventPayload); err != nil || eventPayload.Name != targetName {
		t.Fatal("目标序号 5 的目录内容不符", err)
	}
	if len(pending.devices) != 1 || pending.devices[0].DeviceID != deviceID ||
		pending.devices[0].State.Status != domain.SyncDeviceStatusResetRequired || pending.devices[0].State.AckSequence != 0 {
		t.Fatal("目标写入新目录后应仍待重建")
	}

	// 3. 验证换代后和新基线确认后的旧游标均返回 410。
	signingConfig, err := config.LoadJWTConfig("")
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := service.NewSyncChangeCursorVerifier(signingConfig.SigningKey)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := verifier.Verify(userID, deviceID, oldCursor)
	if err != nil || verified == nil || verified.Sequence != 5 || verified.Generation != backupState.devices[0].State.Generation ||
		verified.Generation == pending.devices[0].State.Generation {
		t.Fatal("当前目标签名配置下的旧游标未通过签名及代次对照", err)
	}
	verifyBackupTargetB5OldCursorRejected(t, ctx, target, fixture, pending, userID, deviceID, oldCursor)
	confirmBackupTargetB5Baseline(t, ctx, target, profile.Data.RootEntryID, "5")
	active := readBackupB5DatabaseState(t, ctx, fixture.database.pool, userID)
	if len(active.devices) != 1 || active.devices[0].UserID != userID || active.devices[0].DeviceID != deviceID ||
		active.devices[0].State.Status != domain.SyncDeviceStatusActive || active.devices[0].State.AckSequence != 5 ||
		active.devices[0].State.Generation == verified.Generation || active.high != 5 || !reflect.DeepEqual(active.events, pending.events) {
		t.Fatal("确认新基线后设备应按新代次活动且确认到序号 5")
	}
	verifyBackupTargetB5OldCursorRejected(t, ctx, target, fixture, active, userID, deviceID, oldCursor)
	logs, err := stopTarget()
	if err != nil {
		t.Fatal("隔离目标关停失败", err)
	}
	verifyBackupSourceStopped(t, target.baseURL, logs)
	t.Logf("B5 源和目标均使用序号 5 但事件内容不同，旧游标签名有效，目标待重建及 ACTIVE/ack=5 时拉取、确认均返回 410 CURSOR_EXPIRED，游标 SHA-256 %x",
		sha256.Sum256([]byte(oldCursor)))
	t.Logf("本轮专用容器 MySQL %s，MinIO %s，后端已关停", source.mysqlContainer, source.minioContainer)
}

func verifyBackupTargetB5OldCursorRejected(t *testing.T, ctx context.Context, target backupSourceClient, fixture backupTargetRemapFixture,
	before backupB5DatabaseState, userID, deviceID uint64, oldCursor string,
) {
	t.Helper()
	activity := readBackupB5SyncDeviceTimes(t, ctx, fixture.database.pool, userID, deviceID)
	for _, path := range []string{"/api/v1/sync/changes/pull", "/api/v1/sync/changes/confirm"} {
		var failure dto.ErrorResponse
		target.request(t, ctx, http.MethodPost, path, "", strings.NewReader(fmt.Sprintf(`{"cursor":%q}`, oldCursor)),
			http.StatusGone, &failure)
		if failure.RequestID == "" || failure.Error.Code != "CURSOR_EXPIRED" {
			t.Fatalf("目标接口 %s 未明确拒绝原代次游标 %q", path, failure.Error.Code)
		}
	}
	if after := readBackupB5DatabaseState(t, ctx, fixture.database.pool, userID); !reflect.DeepEqual(after, before) ||
		readBackupB5SyncDeviceTimes(t, ctx, fixture.database.pool, userID, deviceID) != activity {
		t.Fatal("拒绝旧游标后设备、账目或日志发生变化")
	}
}

func startBackupTargetB5WithSourceSession(t *testing.T, ctx context.Context, fixture backupTargetRemapFixture,
	signingSecret string,
) (backupSourceClient, func() (string, error)) {
	t.Helper()
	if signingSecret == "" || os.Getenv("CLOUDNEST_S3_BUCKET") != fixture.targetBucket.name {
		t.Fatal("隔离目标的签名配置或桶未明确绑定本轮样本")
	}
	address := reserveWebLoginAddress(t)
	t.Setenv("CLOUDNEST_DB_NAME", fixture.database.name)
	t.Setenv("CLOUDNEST_HTTP_ADDR", address)
	t.Setenv("CLOUDNEST_TEMP_DIR", t.TempDir())
	t.Setenv("CLOUDNEST_JWT_SECRET", signingSecret)
	target, stop := startConfiguredBackupFixtureServer(t, ctx, address, run)
	target.accessToken = fixture.source.api.accessToken
	return target, stop
}

func confirmBackupTargetB5Baseline(t *testing.T, ctx context.Context, target backupSourceClient, rootID, sequence string) string {
	t.Helper()
	// 1. 取完整新快照并保存，再用末页凭据确认。
	const path = "/api/v1/sync/snapshots"
	var snapshot dto.SyncSnapshotPageResponse
	target.request(t, ctx, http.MethodPost, path, "b5-target-new-generation-baseline", strings.NewReader("{}"), http.StatusCreated, &snapshot)
	if snapshot.RequestID == "" || snapshot.Data.SnapshotID == "" || snapshot.Data.RootEntryID != rootID ||
		snapshot.Data.HighWaterSequence != sequence || snapshot.Data.NextCursor != nil || snapshot.Data.BaselineToken == nil {
		t.Fatal("未取得目标的完整新快照")
	}
	encoded, err := json.Marshal(snapshot.Data)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(t.TempDir(), "target-sync-baseline.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	var confirmed dto.ConfirmSyncSnapshotBaselineResponse
	target.request(t, ctx, http.MethodPost, path+"/"+snapshot.Data.SnapshotID+"/confirm", "",
		strings.NewReader(fmt.Sprintf(`{"baseline_token":%q}`, *snapshot.Data.BaselineToken)), http.StatusOK, &confirmed)
	if confirmed.RequestID == "" || confirmed.Data.Status != "ACTIVE" || confirmed.Data.AckSequence != sequence {
		t.Fatal("目标新基线确认不符")
	}
	return *snapshot.Data.BaselineToken
}
