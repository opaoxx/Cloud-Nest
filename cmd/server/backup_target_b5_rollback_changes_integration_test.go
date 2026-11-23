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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestServerBackupTargetB5RollbackChangesIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后验证 B5 回退后的增量同步")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 重建隔离回退服务，取得已确认的序号 5 基线。
	running := startBackupTargetB5RollbackBaseline(t, ctx)
	api, pool, snapshot, before := running.api, running.database.pool, running.snapshot, running.state
	device := before.devices[0]
	activityBefore := readBackupB5SyncDeviceTimes(t, ctx, pool, device.UserID, device.DeviceID)

	// 2. 新建合成目录，从基线首次拉取并核对序号 6。
	const directoryName = "B5 回退后新增的合成目录"
	var created dto.CreateDirectoryResponse
	api.request(t, ctx, http.MethodPost, "/api/v1/directories", "b5-rollback-event-six",
		strings.NewReader(fmt.Sprintf(`{"parent_id":%q,"name":%q}`, snapshot.RootEntryID, directoryName)), http.StatusCreated, &created)
	entry := created.Data.Entry
	if created.RequestID == "" || entry.ID == "" || entry.ParentID != snapshot.RootEntryID || entry.Name != directoryName || entry.Revision != "1" {
		t.Fatal("回退服务未成功创建预期合成目录")
	}
	var page dto.PullSyncChangesResponse
	api.request(t, ctx, http.MethodPost, "/api/v1/sync/changes/pull", "",
		strings.NewReader(fmt.Sprintf(`{"baseline_token":%q}`, *snapshot.BaselineToken)), http.StatusOK, &page)
	if page.RequestID == "" || page.Data.HighWaterSequence != "6" || page.Data.HasMore || page.Data.NextCursor == "" ||
		len(page.Data.Events) != 1 || page.Data.Events[0].Sequence != "6" || page.Data.Events[0].EntryID != entry.ID ||
		page.Data.Events[0].Revision != entry.Revision || page.Data.Events[0].EventType != "DIRECTORY_CREATED" {
		t.Fatal("从回退后的序号 5 基线应只收到新增事件 6")
	}
	delivered := page.Data.Events[0]
	wantPayload := map[string]string{"entry_id": entry.ID, "parent_id": entry.ParentID, "name": entry.Name, "revision": entry.Revision}
	var payload map[string]string
	if err := json.Unmarshal(delivered.Payload, &payload); err != nil || !reflect.DeepEqual(payload, wantPayload) {
		t.Fatal("交付事件 6 的目录载荷与正式创建结果不符", err)
	}
	pulled := readBackupB5DatabaseState(t, ctx, pool, device.UserID)
	if pulled.high != 6 || pulled.used != before.used || pulled.reserved != before.reserved ||
		!reflect.DeepEqual(pulled.devices, before.devices) || len(pulled.events) != 6 ||
		!reflect.DeepEqual(pulled.events[:5], before.events) ||
		readBackupB5SyncDeviceTimes(t, ctx, pool, device.UserID, device.DeviceID) != activityBefore {
		t.Fatal("拉取后应保留确认位置 5、活动时间、配额和原五条日志")
	}
	stored := pulled.events[5]
	var compactPayload bytes.Buffer
	if err := json.Compact(&compactPayload, []byte(stored.payload)); err != nil ||
		stored.sequence != 6 || strconv.FormatUint(stored.entryID, 10) != delivered.EntryID ||
		strconv.FormatUint(stored.revision, 10) != delivered.Revision || stored.eventType != delivered.EventType ||
		!bytes.Equal(compactPayload.Bytes(), delivered.Payload) {
		t.Fatal("事件 6 的数据库记录与正式接口交付不一致", err)
	}

	// 3. 保存并读回事件和游标，再提交确认并核对数据库。
	checkpointPath := filepath.Join(t.TempDir(), "rollback-sync-checkpoint-6.json")
	saveBackupTargetB5ChangePage(t, checkpointPath, page.Data)
	persisted := readBackupTargetB5ChangePage(t, checkpointPath)
	if !reflect.DeepEqual(persisted, page.Data) {
		t.Fatal("已保存的事件 6 或游标与接口交付不符")
	}
	signing, err := config.LoadJWTConfig("")
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := service.NewSyncChangeCursorVerifier(signing.SigningKey)
	if err != nil {
		t.Fatal(err)
	}
	position, err := verifier.Verify(device.UserID, device.DeviceID, persisted.NextCursor)
	if err != nil || position == nil || position.Sequence != 6 || position.Generation != device.State.Generation {
		t.Fatal("增量游标未绑定原设备、回退后新代次和序号 6", err)
	}
	var confirmed dto.ConfirmSyncSnapshotBaselineResponse
	api.request(t, ctx, http.MethodPost, "/api/v1/sync/changes/confirm", "",
		strings.NewReader(fmt.Sprintf(`{"cursor":%q}`, persisted.NextCursor)), http.StatusOK, &confirmed)
	if confirmed.RequestID == "" || confirmed.Data.Status != "ACTIVE" || confirmed.Data.AckSequence != "6" {
		t.Fatal("回退服务未确认已保存的序号 6 游标")
	}
	pulled.devices[0].State.AckSequence = 6
	if after := readBackupB5DatabaseState(t, ctx, pool, device.UserID); !reflect.DeepEqual(after, pulled) {
		t.Fatal("确认后的持久位置、代次、六条日志或配额不符")
	}
	logs, err := running.stop()
	if err != nil {
		t.Fatal("隔离回退服务关停失败", err)
	}
	verifyBackupB5ServiceStopped(t, api.baseURL, logs)
	t.Logf("B5 回退后增量同步，设备 %d，基线 %s，唯一新事件 6 条目 %s，确认位置 5→6，已用 %d B，游标 SHA-256 %x",
		device.DeviceID, snapshot.SnapshotID, entry.ID, before.used, sha256.Sum256([]byte(persisted.NextCursor)))
	t.Logf("本轮专用容器 MySQL %s，MinIO %s，后端已关停", running.source.mysqlContainer, running.source.minioContainer)
}
