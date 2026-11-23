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

func TestServerBackupSourceB5PostBackupCursorIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后验证隔离源端备份后的旧代次游标")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 保存已核验的备份和备份前游标。
	source := prepareBackupSource(t, ctx)
	backupDir := t.TempDir()
	manifest, originalCursor := prepareBackupSourceB5ManifestAndCursor(t, ctx, source, backupDir)
	createBackupSourceB5PostBackupCursor(t, ctx, source, backupDir, manifest, originalCursor)
	t.Logf("本轮专用容器 MySQL %s，MinIO %s，后端已关停", source.mysqlContainer, source.minioContainer)
}

func createBackupSourceB5PostBackupCursor(t *testing.T, ctx context.Context, source backupSourceFixture,
	backupDir string, manifest backupSourceManifest, originalCursor string,
) string {
	t.Helper()
	userID := backupFixtureID(t, source.login.User.ID)
	deviceID := backupFixtureID(t, source.login.Device.ID)
	before := readBackupB5DatabaseState(t, ctx, source.pool, userID)
	if before.high != 4 || len(before.events) != 4 || len(before.devices) != 1 || before.devices[0].UserID != userID ||
		before.devices[0].DeviceID != deviceID || before.devices[0].State.Status != domain.SyncDeviceStatusActive ||
		before.devices[0].State.AckSequence != 2 || originalCursor == "" {
		t.Fatal("停写备份前的设备或变更日志位置不符")
	}
	signingConfig, err := config.LoadJWTConfig("")
	if err != nil || os.Getenv("CLOUDNEST_DB_NAME") != source.databaseName || os.Getenv("CLOUDNEST_S3_BUCKET") != source.bucket.name {
		t.Fatal("重启参数未绑定到本轮隔离源端", err)
	}
	verifier, err := service.NewSyncChangeCursorVerifier(signingConfig.SigningKey)
	if err != nil {
		t.Fatal(err)
	}

	// 2. 重启隔离源端，新增一条备份后的变更。
	address := reserveWebLoginAddress(t)
	t.Setenv("CLOUDNEST_HTTP_ADDR", address)
	restarted, stop := startConfiguredBackupFixtureServer(t, ctx, address, run)
	restarted.accessToken = source.api.accessToken
	pull := func(cursor string) dto.PullSyncChangesResponse {
		t.Helper()
		var pulled dto.PullSyncChangesResponse
		restarted.request(t, ctx, http.MethodPost, "/api/v1/sync/changes/pull", "",
			strings.NewReader(fmt.Sprintf(`{"cursor":%q}`, cursor)), http.StatusOK, &pulled)
		if pulled.RequestID == "" || pulled.Data.NextCursor == "" || pulled.Data.HasMore {
			t.Fatal("隔离源端未交付完整增量页和续读游标")
		}
		return pulled
	}
	pinned := pull(originalCursor)
	if pinned.Data.HighWaterSequence != "4" || len(pinned.Data.Events) != 2 ||
		pinned.Data.Events[0].Sequence != "3" || pinned.Data.Events[1].Sequence != "4" {
		t.Fatal("源端重启后应先从备份前游标读到序号 3 和 4")
	}
	const postBackupName = "源端备份后目录"
	var created dto.CreateDirectoryResponse
	restarted.request(t, ctx, http.MethodPost, "/api/v1/directories", "b5-source-after-backup-directory",
		strings.NewReader(fmt.Sprintf(`{"parent_id":%q,"name":%q}`, source.login.RootEntryID, postBackupName)),
		http.StatusCreated, &created)
	page := pull(pinned.Data.NextCursor)
	if page.Data.HighWaterSequence != "5" || len(page.Data.Events) != 1 || page.Data.Events[0].Sequence != "5" ||
		page.Data.Events[0].EntryID != created.Data.Entry.ID || page.Data.Events[0].EventType != "DIRECTORY_CREATED" {
		t.Fatal("未从原代次交付备份之后的目录事件")
	}
	content, err := json.Marshal(page.Data)
	if err != nil {
		t.Fatal(err)
	}
	cursorPath := filepath.Join(t.TempDir(), "source-post-backup-page.json")
	if err := os.WriteFile(cursorPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(cursorPath)
	if err != nil {
		t.Fatal(err)
	}
	var persisted dto.PullSyncChangesData
	if err := json.Unmarshal(saved, &persisted); err != nil || persisted.NextCursor != page.Data.NextCursor {
		t.Fatal("读回备份后增量页失败", err)
	}

	// 3. 验证新交付的游标仍属于原代次，并确认备份摘要一致。
	position, err := verifier.Verify(userID, deviceID, persisted.NextCursor)
	if err != nil || position == nil || position.Sequence != 5 || position.Generation != before.devices[0].State.Generation {
		t.Fatal("备份后的游标验签失败，或未绑定序号 5 与原代次", err)
	}
	var confirmed dto.ConfirmSyncSnapshotBaselineResponse
	restarted.request(t, ctx, http.MethodPost, "/api/v1/sync/changes/confirm", "",
		strings.NewReader(fmt.Sprintf(`{"cursor":%q}`, persisted.NextCursor)), http.StatusOK, &confirmed)
	if confirmed.RequestID == "" || confirmed.Data.Status != "ACTIVE" || confirmed.Data.AckSequence != "5" {
		t.Fatal("同一合成设备未能确认备份后的旧代次游标")
	}
	if resumed := pull(persisted.NextCursor); resumed.Data.HighWaterSequence != "5" ||
		resumed.Data.Events == nil || len(resumed.Data.Events) != 0 {
		t.Fatal("备份后的旧代次游标在源端应可续读空页")
	}
	after := readBackupB5DatabaseState(t, ctx, source.pool, userID)
	expectedDevice := before.devices[0]
	expectedDevice.State.AckSequence = 5
	if after.high != 5 || after.used != before.used || after.reserved != before.reserved ||
		len(after.devices) != 1 || after.devices[0] != expectedDevice || len(after.events) != 5 ||
		!reflect.DeepEqual(after.events[:4], before.events) || after.events[4].sequence != 5 {
		t.Fatal("源端备份后状态应只追加序号 5 并确认到该位置")
	}
	sqlBackup, err := os.ReadFile(filepath.Join(backupDir, "database.sql"))
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(sqlBackup)) != manifest.DatabaseSHA256 {
		t.Fatal("源端备份后写入改变了已经保存的 SQL 摘要", err)
	}
	logs, err := stop()
	if err != nil {
		t.Fatal("备份后的隔离源端关停失败", err)
	}
	verifyBackupSourceStopped(t, restarted.baseURL, logs)
	t.Logf("B5 源端备份上界 4，重启后序号 5 的原代次游标验签、拉取和确认成功，设备 %d，游标 SHA-256 %x，原 SQL SHA-256 %s",
		deviceID, sha256.Sum256([]byte(persisted.NextCursor)), manifest.DatabaseSHA256)
	return persisted.NextCursor
}
