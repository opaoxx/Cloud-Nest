package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestServerBackupTargetB5OldSnapshotIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后复验恢复目标拒绝旧快照凭据")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 在隔离源端读完两页快照并确认末页，保留两种凭据。
	source := prepareBackupSource(t, ctx)
	signingSecret := os.Getenv("CLOUDNEST_JWT_SECRET")
	firstPage, lastPage := confirmBackupSourceB5TwoPageSnapshot(t, ctx, source)
	userID := backupFixtureID(t, source.login.User.ID)
	deviceID := backupFixtureID(t, source.login.Device.ID)
	backupDir := t.TempDir()
	fileFixture := prepareReplacementPublicationFixtureFromSource(t, ctx, source)
	verifyBackupSourceFileVersions(t, ctx, fileFixture)
	dump := stopAndExportBackupSourceB5SQL(t, ctx, fileFixture, backupDir)
	manifest := writeAndVerifyBackupSourceB5Manifest(t, ctx, fileFixture, backupDir, dump)
	backupState := readBackupB5DatabaseState(t, ctx, source.pool, userID)

	// 2. 还原同一备份并离线换代，核对旧凭据仍可验签且尚未到期。
	fixture := restoreBackupTargetB5Remap(t, ctx, source, backupDir, manifest, backupState)
	verifyAndApplyBackupTargetB5ObjectMap(t, ctx, fixture)
	resetBackupTargetB5SyncDevices(t, ctx, fixture)
	target, stopTarget := startBackupTargetB5WithSourceSession(t, ctx, fixture, signingSecret)
	var profile dto.UserProfileResponse
	target.request(t, ctx, http.MethodGet, "/api/v1/me", "", nil, http.StatusOK, &profile)
	if profile.RequestID == "" || profile.Data.User.ID != source.login.User.ID || profile.Data.RootEntryID != source.login.RootEntryID {
		t.Fatal("目标未接受恢复出的同一合成会话")
	}
	oldGeneration := backupState.devices[0].State.Generation
	signingConfig, err := config.LoadJWTConfig("")
	if err != nil {
		t.Fatal(err)
	}
	verifyBackupB5OldSnapshotSignatures(t, signingConfig.SigningKey, userID, deviceID, oldGeneration, firstPage, lastPage)
	pending := readBackupB5DatabaseState(t, ctx, fixture.database.pool, userID)
	if len(pending.devices) != 1 || pending.devices[0].DeviceID != deviceID ||
		pending.devices[0].State.Status != domain.SyncDeviceStatusResetRequired || pending.devices[0].State.AckSequence != 0 ||
		pending.devices[0].State.Generation == oldGeneration || pending.high != 4 || !reflect.DeepEqual(pending.events, backupState.events) {
		t.Fatal("换代目标应待重建，确认位置为零，并保留备份点的四条日志")
	}

	// 3. 在目标待重建和确认新基线后分别提交旧凭据，核对 410 和数据库状态。
	verifyBackupTargetB5OldSnapshotRejected(t, ctx, target, fixture, pending, userID, deviceID, firstPage, lastPage)
	confirmBackupTargetB5Baseline(t, ctx, target, profile.Data.RootEntryID, "4")
	active := readBackupB5DatabaseState(t, ctx, fixture.database.pool, userID)
	if len(active.devices) != 1 || active.devices[0].DeviceID != deviceID ||
		active.devices[0].State.Status != domain.SyncDeviceStatusActive || active.devices[0].State.AckSequence != 4 ||
		active.devices[0].State.Generation == oldGeneration || active.high != 4 || !reflect.DeepEqual(active.events, pending.events) {
		t.Fatal("目标应以新代次确认到序号 4，并保留原日志")
	}
	verifyBackupTargetB5OldSnapshotRejected(t, ctx, target, fixture, active, userID, deviceID, firstPage, lastPage)
	logs, err := stopTarget()
	if err != nil {
		t.Fatal("隔离目标关停失败", err)
	}
	verifyBackupSourceStopped(t, target.baseURL, logs)
	t.Logf("B5 旧快照 %s 在源端续页和确认均为 200，目标待重建及 ACTIVE/ack=4 时两接口均返回 410 CURSOR_EXPIRED，续页 SHA-256 %x，末页 SHA-256 %x",
		firstPage.SnapshotID, sha256.Sum256([]byte(*firstPage.NextCursor)), sha256.Sum256([]byte(*lastPage.BaselineToken)))
	t.Logf("本轮专用容器 MySQL %s，MinIO %s，后端已关停", source.mysqlContainer, source.minioContainer)
}

func confirmBackupSourceB5TwoPageSnapshot(t *testing.T, ctx context.Context, source backupSourceFixture) (dto.SyncSnapshotPageData, dto.SyncSnapshotPageData) {
	t.Helper()
	// 1. 快照前提交两个目录，每页一项以取得两类正式凭据。
	var directoryIDs [2]string
	for index, name := range []string{"备份快照首页目录", "备份快照末页目录"} {
		var directory dto.CreateDirectoryResponse
		source.api.request(t, ctx, http.MethodPost, "/api/v1/directories", fmt.Sprintf("b5-old-snapshot-directory-%d", index),
			strings.NewReader(fmt.Sprintf(`{"parent_id":%q,"name":%q}`, source.login.RootEntryID, name)), http.StatusCreated, &directory)
		directoryIDs[index] = directory.Data.Entry.ID
	}
	const snapshots = "/api/v1/sync/snapshots"
	var first, last dto.SyncSnapshotPageResponse
	source.api.request(t, ctx, http.MethodPost, snapshots, "b5-source-two-page-snapshot", strings.NewReader(`{"page_size":1}`), http.StatusCreated, &first)
	if first.RequestID == "" || first.Data.SnapshotID == "" || first.Data.NextCursor == nil || *first.Data.NextCursor == "" ||
		first.Data.BaselineToken != nil || first.Data.RootEntryID != source.login.RootEntryID || first.Data.HighWaterSequence != "2" {
		t.Fatal("源端未交付可续页、截止序号为 2 的快照首页")
	}
	path := snapshots + "/" + first.Data.SnapshotID
	source.api.request(t, ctx, http.MethodGet, path+"?cursor="+url.QueryEscape(*first.Data.NextCursor), "", nil, http.StatusOK, &last)
	if last.RequestID == "" || last.Data.SnapshotID != first.Data.SnapshotID || last.Data.NextCursor != nil ||
		last.Data.BaselineToken == nil || *last.Data.BaselineToken == "" || last.Data.RootEntryID != first.Data.RootEntryID ||
		last.Data.HighWaterSequence != first.Data.HighWaterSequence || !last.Data.ExpiresAt.Equal(first.Data.ExpiresAt) {
		t.Fatal("源端未按同一快照交付末页确认凭据")
	}

	// 2. 先保存并读回两页，核对条目后提交末页凭据。
	pages := [2]dto.SyncSnapshotPageData{first.Data, last.Data}
	saved, err := json.Marshal(pages)
	if err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(t.TempDir(), "source-sync-snapshot-pages.json")
	if err := os.WriteFile(artifact, saved, 0o600); err != nil {
		t.Fatal(err)
	}
	readback, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatal(err)
	}
	var persisted [2]dto.SyncSnapshotPageData
	if err := json.Unmarshal(readback, &persisted); err != nil || !reflect.DeepEqual(persisted, pages) {
		t.Fatal("快照两页与临时目录读回不一致", err)
	}
	for index, page := range persisted {
		var item struct {
			ID string `json:"entry_id"`
		}
		if len(page.Items) != 1 || json.Unmarshal(page.Items[0], &item) != nil || item.ID != directoryIDs[index] || item.ID == "" {
			t.Fatalf("源端快照第 %d 页未包含预期目录", index+1)
		}
	}
	var confirmed dto.ConfirmSyncSnapshotBaselineResponse
	source.api.request(t, ctx, http.MethodPost, path+"/confirm", "",
		strings.NewReader(fmt.Sprintf(`{"baseline_token":%q}`, *persisted[1].BaselineToken)), http.StatusOK, &confirmed)
	if confirmed.RequestID == "" || confirmed.Data.Status != "ACTIVE" || confirmed.Data.AckSequence != "2" {
		t.Fatal("源端未确认完整的两页快照")
	}
	return persisted[0], persisted[1]
}

func verifyBackupB5OldSnapshotSignatures(t *testing.T, key []byte, userID, deviceID uint64,
	generation domain.SyncCursorGeneration, firstPage, lastPage dto.SyncSnapshotPageData,
) {
	t.Helper()
	// 1. 用目标实际加载的密钥及同一身份校验两种凭据的签名和时限。
	snapshotID, err := uuid.Parse(firstPage.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	cursorVerifier, err := service.NewSyncSnapshotCursorVerifier(key)
	if err != nil {
		t.Fatal(err)
	}
	baselineVerifier, err := service.NewSyncSnapshotBaselineVerifier(key)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	cursor, err := cursorVerifier.Verify(userID, deviceID, snapshotID, *firstPage.NextCursor, now)
	if err != nil || cursor == nil || cursor.Binding.Generation != generation || cursor.Binding.PageSize != 1 ||
		cursor.NextPosition != 1 || !cursor.ExpiresAt.Equal(firstPage.ExpiresAt) {
		t.Fatal("旧续页凭据未通过目标签名、备份代次、位置和时限校验", err)
	}
	baseline, err := baselineVerifier.Verify(userID, deviceID, snapshotID, *lastPage.BaselineToken, now)
	if err != nil || baseline == nil || baseline.Generation != generation ||
		baseline.HighWaterSequence != 2 || !baseline.ExpiresAt.Equal(lastPage.ExpiresAt) {
		t.Fatal("旧末页凭据未通过目标签名、备份代次、截止序号和时限校验", err)
	}
}

func verifyBackupTargetB5OldSnapshotRejected(t *testing.T, ctx context.Context, target backupSourceClient, fixture backupTargetRemapFixture,
	before backupB5DatabaseState, userID, deviceID uint64, firstPage, lastPage dto.SyncSnapshotPageData,
) {
	t.Helper()
	// 1. 分别请求旧续页与旧确认，确认请求完成时仍早于原期限。
	activity := readBackupB5SyncDeviceTimes(t, ctx, fixture.database.pool, userID, deviceID)
	path := "/api/v1/sync/snapshots/" + firstPage.SnapshotID
	var continuationFailure, confirmationFailure dto.ErrorResponse
	target.request(t, ctx, http.MethodGet, path+"?cursor="+url.QueryEscape(*firstPage.NextCursor), "", nil, http.StatusGone, &continuationFailure)
	target.request(t, ctx, http.MethodPost, path+"/confirm", "", strings.NewReader(fmt.Sprintf(`{"baseline_token":%q}`, *lastPage.BaselineToken)),
		http.StatusGone, &confirmationFailure)
	if continuationFailure.RequestID == "" || continuationFailure.Error.Code != "CURSOR_EXPIRED" ||
		confirmationFailure.RequestID == "" || confirmationFailure.Error.Code != "CURSOR_EXPIRED" ||
		!time.Now().Before(firstPage.ExpiresAt) {
		t.Fatal("目标未在原期限内按契约拒绝旧续页和旧末页确认")
	}

	// 2. 比对拒绝前后的持久状态与设备活动时间。
	if after := readBackupB5DatabaseState(t, ctx, fixture.database.pool, userID); !reflect.DeepEqual(after, before) ||
		readBackupB5SyncDeviceTimes(t, ctx, fixture.database.pool, userID, deviceID) != activity {
		t.Fatal("拒绝旧快照后设备、账目或日志发生变化")
	}
}
