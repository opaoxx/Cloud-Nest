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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestServerBackupTargetB5RollbackBaselineIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后验证 B5 回退后的同步基线")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 启动已换代的隔离回退服务，沿用原设备登录。
	sample := prepareBackupTargetB5RollbackSample(t, ctx)
	backupDir := t.TempDir()
	manifest := exportBackupTargetB5RollbackBundle(t, ctx, sample, backupDir)
	database := restoreBackupTargetB5RollbackDatabase(t, ctx, sample, backupDir, manifest)
	bucket, mapping := restoreBackupTargetB5RollbackObjects(t, ctx, sample, backupDir, manifest)
	verifyAndApplyBackupTargetB5RollbackObjectMap(t, ctx, sample, database, bucket, manifest, mapping)
	resetBackupTargetB5RollbackSyncDevices(t, ctx, sample, database, bucket, manifest)

	source := sample.fixture.source
	userID := backupFixtureID(t, source.login.User.ID)
	deviceID := backupFixtureID(t, source.login.Device.ID)
	before := readBackupB5DatabaseState(t, ctx, database.pool, userID)
	if len(before.devices) != 1 || before.devices[0].DeviceID != deviceID ||
		before.devices[0].State.Status != domain.SyncDeviceStatusResetRequired || before.devices[0].State.AckSequence != 0 ||
		before.high != 5 {
		t.Fatal("回退库未保留原待重建设备和截止序号 5")
	}
	var installationID string
	if err := source.pool.QueryRowContext(ctx, "SELECT installation_id FROM user_devices WHERE user_id=? AND id=?", userID, deviceID).
		Scan(&installationID); err != nil || installationID == "" {
		t.Fatal("未取得原合成设备的安装标识", err)
	}
	api, stop := startBackupFixtureServer(t, ctx, database.name)
	login := loginBackupFixtureWithInstallation(t, ctx, &api, os.Getenv("CLOUDNEST_ADMIN_PASSWORD"), installationID)
	if login.User.ID != source.login.User.ID || login.Device.ID != source.login.Device.ID ||
		login.RootEntryID != source.login.RootEntryID || login.Session.ID == source.login.Session.ID {
		t.Fatal("回退登录未为原设备创建新的会话")
	}
	if afterLogin := readBackupB5DatabaseState(t, ctx, database.pool, userID); !reflect.DeepEqual(afterLogin, before) {
		t.Fatal("原设备重新登录改变了待重建基线或业务状态")
	}

	// 2. 取得并读回完整快照，核对两个目录与当前 v3。
	snapshot := saveBackupTargetB5RollbackSnapshot(t, ctx, api, login.RootEntryID, manifest)
	pending := readBackupB5DatabaseState(t, ctx, database.pool, userID)
	if len(pending.devices) != 1 || pending.devices[0].DeviceID != deviceID ||
		pending.devices[0].State.Status != domain.SyncDeviceStatusResetRequired || pending.devices[0].State.AckSequence != 0 ||
		pending.devices[0].State.Generation == before.devices[0].State.Generation {
		t.Fatal("保存快照后应仍待重建，且已产生用于新快照的代次")
	}
	signing, err := config.LoadJWTConfig("")
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := service.NewSyncSnapshotBaselineVerifier(signing.SigningKey)
	if err != nil {
		t.Fatal(err)
	}
	snapshotID, err := uuid.Parse(snapshot.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := verifier.Verify(userID, deviceID, snapshotID, *snapshot.BaselineToken, time.Now())
	if err != nil || baseline == nil || baseline.HighWaterSequence != 5 ||
		baseline.Generation != pending.devices[0].State.Generation || !baseline.ExpiresAt.Equal(snapshot.ExpiresAt) {
		t.Fatal("从磁盘读回的末页凭据未绑定原设备、序号 5 和当前快照代次", err)
	}

	// 3. 用已保存的末页凭据确认到序号 5，复核持久状态。
	var confirmation dto.ConfirmSyncSnapshotBaselineResponse
	api.request(t, ctx, http.MethodPost, "/api/v1/sync/snapshots/"+snapshot.SnapshotID+"/confirm", "",
		strings.NewReader(fmt.Sprintf(`{"baseline_token":%q}`, *snapshot.BaselineToken)), http.StatusOK, &confirmation)
	if confirmation.RequestID == "" || confirmation.Data.Status != "ACTIVE" || confirmation.Data.AckSequence != "5" {
		t.Fatal("回退服务未确认已保存的基线到序号 5")
	}
	expected := before
	expected.devices = pending.devices
	expected.devices[0].State.Status = domain.SyncDeviceStatusActive
	expected.devices[0].State.AckSequence = 5
	if after := readBackupB5DatabaseState(t, ctx, database.pool, userID); !reflect.DeepEqual(after, expected) {
		t.Fatal("确认后的设备代次、日志或配额与预期不符")
	}
	logs, err := stop()
	if err != nil {
		t.Fatal("隔离回退服务关停失败", err)
	}
	verifyBackupB5ServiceStopped(t, api.baseURL, logs)
	t.Logf("B5 回退服务新基线，设备 %d，快照 %s，%d 个条目含 v3，确认位置 5，日志 1～5，已用 %d B，末页凭据 SHA-256 %x",
		deviceID, snapshot.SnapshotID, len(snapshot.Items), before.used, sha256.Sum256([]byte(*snapshot.BaselineToken)))
	t.Logf("本轮专用容器 MySQL %s，MinIO %s，后端已关停", source.mysqlContainer, source.minioContainer)
}

func saveBackupTargetB5RollbackSnapshot(t *testing.T, ctx context.Context, api backupSourceClient, rootID string,
	manifest backupSourceManifest,
) dto.SyncSnapshotPageData {
	t.Helper()
	// 1. 通过正式接口取得完整快照，写入临时文件并读回。
	var snapshot dto.SyncSnapshotPageResponse
	api.request(t, ctx, http.MethodPost, "/api/v1/sync/snapshots", "b5-rollback-new-baseline", strings.NewReader("{}"), http.StatusCreated, &snapshot)
	if snapshot.RequestID == "" || snapshot.Data.SnapshotID == "" || snapshot.Data.RootEntryID != rootID ||
		snapshot.Data.HighWaterSequence != "5" || snapshot.Data.NextCursor != nil ||
		snapshot.Data.BaselineToken == nil || *snapshot.Data.BaselineToken == "" {
		t.Fatal("回退服务未交付截止序号为 5 的完整快照")
	}
	content, err := json.Marshal(snapshot.Data)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "rollback-sync-baseline.json")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted dto.SyncSnapshotPageData
	if err := json.Unmarshal(saved, &persisted); err != nil || !reflect.DeepEqual(persisted, snapshot.Data) {
		t.Fatal("磁盘快照与正式接口交付不一致", err)
	}

	// 2. 用备份清单核对快照中的每个目录和当前文件元数据。
	current := manifest.FileReferences[0]
	object := manifest.Objects[0]
	if current.Revision != 3 || current.SizeBytes != 40 || current.ParentID == nil || current.ObjectID != object.ID {
		t.Fatal("回退备份缺少预期的当前 v3 文件")
	}
	currentID := strconv.FormatUint(current.ID, 10)
	expected := map[string]dto.SyncSnapshotEntry{
		currentID: {EntryID: currentID, ParentID: strconv.FormatUint(*current.ParentID, 10), Name: current.Name,
			EntryType: "FILE", Revision: "3", SizeBytes: 40, SHA256: &object.SHA256},
	}
	for _, directory := range manifest.Directories {
		if directory.ParentID == nil {
			continue
		}
		id := strconv.FormatUint(directory.ID, 10)
		expected[id] = dto.SyncSnapshotEntry{EntryID: id, ParentID: strconv.FormatUint(*directory.ParentID, 10), Name: directory.Name,
			EntryType: "DIR", Revision: strconv.FormatUint(directory.Revision, 10)}
	}
	if len(expected) != 3 || len(persisted.Items) != len(expected) {
		t.Fatal("快照没有完整交付两个目录和一个当前文件")
	}
	for _, encoded := range persisted.Items {
		var item dto.SyncSnapshotEntry
		if err := json.Unmarshal(encoded, &item); err != nil {
			t.Fatal(err)
		}
		want, found := expected[item.EntryID]
		if !found || !reflect.DeepEqual(item, want) {
			t.Fatal("已保存的快照条目与切换后备份中的元数据不符", item.EntryID)
		}
		delete(expected, item.EntryID)
	}
	return persisted
}
