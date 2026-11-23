package main

import (
	"context"
	"crypto/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"golang.org/x/crypto/bcrypt"
)

func TestServerRevokedDeviceSyncRecoveryIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_REVOKED_DEVICE_SYNC_RECOVERY_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_REVOKED_DEVICE_SYNC_RECOVERY_TEST=1 后使用独占 MySQL 和回环服务")
	}
	for _, scenario := range []struct {
		name       string
		historical bool
	}{
		{name: "revoked through current API"},
		{name: "historically revoked", historical: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
			defer cancel()

			// 1. 正式登录并取得可用的同步游标。
			pool, databaseName := prepareServerIntegrationDatabase(t, ctx)
			password := rand.Text()
			hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash) VALUES(1,'root','撤销恢复夹具',?)", string(hash)); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR')"); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CLOUDNEST_STORAGE_PROVIDER", "local_minio")
			prepareServerReadOnlyStorageStub(t, "fixture-revoked-device-sync")
			client, stop := startBackupFixtureServer(t, ctx, databaseName)
			installation := uuid.NewString()
			original := loginBackupFixtureWithInstallation(t, ctx, &client, password, installation)
			userID, deviceID := backupFixtureID(t, original.User.ID), backupFixtureID(t, original.Device.ID)
			originalSession := backupFixtureID(t, original.Session.ID)
			oldClient := client

			const snapshotPath = "/api/v1/sync/snapshots"
			const pullPath = "/api/v1/sync/changes/pull"
			const confirmPath = "/api/v1/sync/changes/confirm"
			body := func(field, token string) *strings.Reader {
				return strings.NewReader(`{"` + field + `":` + strconv.Quote(token) + `}`)
			}
			confirm := func(path, field, token, position string) {
				t.Helper()
				var response dto.ConfirmSyncSnapshotBaselineResponse
				client.request(t, ctx, http.MethodPost, path, "", body(field, token), http.StatusOK, &response)
				if response.RequestID == "" || response.Data.Status != "ACTIVE" || response.Data.AckSequence != position {
					t.Fatalf("同步确认响应不符：%+v，期望位置 %s", response, position)
				}
			}
			establishBaseline := func(key, position string) string {
				t.Helper()
				var snapshot dto.SyncSnapshotPageResponse
				client.request(t, ctx, http.MethodPost, snapshotPath, key, strings.NewReader(`{}`), http.StatusCreated, &snapshot)
				if snapshot.RequestID == "" || snapshot.Data.SnapshotID == "" ||
					snapshot.Data.HighWaterSequence != position || snapshot.Data.RootEntryID != original.RootEntryID ||
					snapshot.Data.BaselineToken == nil || snapshot.Data.NextCursor != nil {
					t.Fatalf("未取得位置 %s 的完整同步快照", position)
				}
				token := *snapshot.Data.BaselineToken
				confirm(snapshotPath+"/"+snapshot.Data.SnapshotID+"/confirm", "baseline_token", token, position)
				return token
			}
			baseline := establishBaseline("revoke-sync-initial-baseline", "0")
			var added dto.CreateDirectoryResponse
			client.request(t, ctx, http.MethodPost, "/api/v1/directories", "revoke-sync-fixture-directory",
				strings.NewReader(`{"parent_id":`+strconv.Quote(original.RootEntryID)+`,"name":"撤销前目录"}`), http.StatusCreated, &added)
			var delivered dto.PullSyncChangesResponse
			client.request(t, ctx, http.MethodPost, pullPath, "", body("baseline_token", baseline), http.StatusOK, &delivered)
			if delivered.RequestID == "" || delivered.Data.NextCursor == "" || delivered.Data.HighWaterSequence != "1" ||
				len(delivered.Data.Events) != 1 || delivered.Data.Events[0].Sequence != "1" || delivered.Data.Events[0].EntryID != added.Data.Entry.ID {
				t.Fatal("撤销前未取得已交付位置 1 的游标")
			}
			oldCursor := delivered.Data.NextCursor
			confirm(confirmPath, "cursor", oldCursor, "1")

			type deviceState struct {
				status     domain.SyncDeviceStatus
				ack        uint64
				generation string
				lastSeenAt time.Time
				updatedAt  time.Time
			}
			readState := func() deviceState {
				t.Helper()
				var state deviceState
				if err := pool.QueryRowContext(ctx, `SELECT status,ack_sequence,HEX(cursor_generation),last_seen_at,updated_at
					FROM sync_devices WHERE user_id=? AND device_id=?`, userID, deviceID).
					Scan(&state.status, &state.ack, &state.generation, &state.lastSeenAt, &state.updatedAt); err != nil {
					t.Fatal("读取原设备的同步状态失败", err)
				}
				return state
			}
			before := readState()
			if before.status != domain.SyncDeviceStatusActive || before.ack != 1 {
				t.Fatalf("旧会话未确认到位置 1：%+v", before)
			}

			// 2. 撤销设备或准备历史撤销状态，再用同一安装标识登录。
			if scenario.historical {
				legacy, err := pool.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer legacy.Rollback()
				if _, err := legacy.ExecContext(ctx, "UPDATE user_devices SET revoked_at=UTC_TIMESTAMP(6) WHERE user_id=? AND id=?", userID, deviceID); err != nil {
					t.Fatal(err)
				}
				if _, err := legacy.ExecContext(ctx, "UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE user_id=? AND device_id=? AND id=?", userID, deviceID, originalSession); err != nil {
					t.Fatal(err)
				}
				if err := legacy.Commit(); err != nil {
					t.Fatal(err)
				}
				if readState() != before {
					t.Fatal("历史撤销夹具应保留同步状态")
				}
			} else {
				var revoked dto.RevokeDeviceResponse
				client.request(t, ctx, http.MethodDelete, "/api/v1/devices/"+original.Device.ID, "", nil, http.StatusOK, &revoked)
				after := readState()
				if !revoked.Data.Revoked || revoked.RequestID == "" || after.status != domain.SyncDeviceStatusResetRequired ||
					after.ack != 0 || after.generation == before.generation {
					t.Fatalf("正式设备撤销未提交同步失效：%+v", after)
				}
			}

			assertOldSessionRevoked := func() {
				t.Helper()
				var failure dto.ErrorResponse
				oldClient.request(t, ctx, http.MethodGet, "/api/v1/me", "", nil, http.StatusUnauthorized, &failure)
				if failure.Error.Code != "SESSION_REVOKED" || failure.RequestID == "" {
					t.Fatalf("旧会话未按撤销状态拒绝：%+v", failure)
				}
			}
			assertOldSessionRevoked()
			client.accessToken = ""
			relogged := loginBackupFixtureWithInstallation(t, ctx, &client, password, installation)
			if relogged.Device.ID != original.Device.ID || relogged.Session.ID == original.Session.ID ||
				relogged.User.ID != original.User.ID || relogged.Session.DeviceID != original.Device.ID {
				t.Fatal("重新登录未复用账号及设备，或未建立新会话")
			}
			assertOldSessionRevoked()
			var profile dto.UserProfileResponse
			client.request(t, ctx, http.MethodGet, "/api/v1/me", "", nil, http.StatusOK, &profile)
			if profile.Data.User.ID != original.User.ID {
				t.Fatal("新会话未通过已恢复设备的权限校验")
			}

			// 3. 核对旧会话、旧游标及新基线的响应。
			reset := readState()
			if reset.status != domain.SyncDeviceStatusResetRequired || reset.ack != 0 ||
				reset.generation == before.generation || !reset.lastSeenAt.Equal(before.lastSeenAt) {
				t.Fatalf("重新登录未保持同步失效：%+v", reset)
			}
			assertOldCursorExpired := func() {
				t.Helper()
				state := readState()
				for _, path := range []string{pullPath, confirmPath} {
					var failure dto.ErrorResponse
					client.request(t, ctx, http.MethodPost, path, "", body("cursor", oldCursor), http.StatusGone, &failure)
					if failure.Error.Code != "CURSOR_EXPIRED" || failure.RequestID == "" || readState() != state {
						t.Fatalf("旧游标没有安全失效：%s，%+v", path, failure)
					}
				}
			}
			assertOldCursorExpired()
			newBaseline := establishBaseline("revoke-sync-next-baseline", "1")
			assertOldCursorExpired()

			var resumed dto.PullSyncChangesResponse
			client.request(t, ctx, http.MethodPost, pullPath, "", body("baseline_token", newBaseline), http.StatusOK, &resumed)
			if resumed.RequestID == "" || resumed.Data.NextCursor == "" || resumed.Data.NextCursor == oldCursor ||
				resumed.Data.HighWaterSequence != "1" || resumed.Data.HasMore || resumed.Data.Events == nil || len(resumed.Data.Events) != 0 {
				t.Fatal("新基线未恢复同步拉取")
			}
			confirm(confirmPath, "cursor", resumed.Data.NextCursor, "1")
			if active := readState(); active.status != domain.SyncDeviceStatusActive || active.ack != 1 ||
				active.generation == before.generation || active.generation == reset.generation {
				t.Fatalf("新代次没有恢复可确认状态：%+v", active)
			}
			if _, err := stop(); err != nil {
				t.Fatal("隔离服务停止失败", err)
			}
			t.Logf("独占库 %s，回环服务 %s。%s：设备 ID %d 复用，旧会话 401、旧拉取和确认 410，新基线确认到 1",
				databaseName, client.baseURL, scenario.name, deviceID)
		})
	}
}
