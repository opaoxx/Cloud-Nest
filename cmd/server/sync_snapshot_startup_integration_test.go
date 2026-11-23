package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"golang.org/x/crypto/bcrypt"
)

func TestServerSyncSnapshotStartupIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_SYNC_SNAPSHOT_STARTUP_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_SYNC_SNAPSHOT_STARTUP_TEST=1 后用独占库和回环存储桩验收")
	}

	// 1. 迁移专用库，准备合成账号和三项能够跨页的活动目录。
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	pool, databaseName := prepareServerIntegrationDatabase(t, ctx)
	password := rand.Text()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash,storage_quota) VALUES(1,'root','快照测试用户',?,100)", string(hash)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES
		(11,1,NULL,'',X'','DIR'),(12,1,11,'目录一',CAST('目录一' AS BINARY),'DIR'),
		(13,1,11,'目录二',CAST('目录二' AS BINARY),'DIR'),(14,1,12,'目录三',CAST('目录三' AS BINARY),'DIR')`); err != nil {
		t.Fatal(err)
	}
	prepareServerReadOnlyStorageStub(t, "fixture-sync-snapshot-startup")
	api, stop := startBackupFixtureServer(t, ctx, databaseName)
	login := loginBackupFixture(t, ctx, &api, password)

	// 2. 通过实际启动的正式路径创建首页并同键重试。
	const path = "/api/v1/sync/snapshots"
	const requestBody = `{"page_size":1}`
	const idempotencyKey = "startup-sync-snapshot-fixture"
	var first, repeated dto.SyncSnapshotPageResponse
	api.request(t, ctx, http.MethodPost, path, idempotencyKey, strings.NewReader(requestBody), http.StatusCreated, &first)
	api.request(t, ctx, http.MethodPost, path, idempotencyKey, strings.NewReader(requestBody), http.StatusCreated, &repeated)
	if first.RequestID == "" || repeated.RequestID == "" || first.RequestID == repeated.RequestID ||
		first.Data.SnapshotID == "" || first.Data.RootEntryID != login.RootEntryID || first.Data.HighWaterSequence != "0" ||
		first.Data.ExpiresAt.IsZero() || first.Data.NextCursor == nil || first.Data.BaselineToken != nil ||
		!reflect.DeepEqual(first.Data, repeated.Data) {
		t.Fatalf("首次创建与同键重试的首页不符：%+v，%+v", first, repeated)
	}

	// 3. 经正式目录接口产生变更，续页仍交付创建时固定的树和截止序号。
	var added dto.CreateDirectoryResponse
	api.request(t, ctx, http.MethodPost, "/api/v1/directories", "startup-sync-fixture-directory",
		strings.NewReader(`{"parent_id":"11","name":"快照之后创建"}`), http.StatusCreated, &added)
	page := first
	for index, wantID := range []string{"12", "13", "14"} {
		if len(page.Data.Items) != 1 {
			t.Fatalf("第 %d 页条目数不符：%+v", index+1, page.Data)
		}
		var entry struct {
			ID string `json:"entry_id"`
		}
		if err := json.Unmarshal(page.Data.Items[0], &entry); err != nil || entry.ID != wantID ||
			page.Data.SnapshotID != first.Data.SnapshotID || page.Data.RootEntryID != first.Data.RootEntryID ||
			page.Data.HighWaterSequence != first.Data.HighWaterSequence || !page.Data.ExpiresAt.Equal(first.Data.ExpiresAt) {
			t.Fatalf("第 %d 页未沿用创建时的内容与头部：%+v，%v", index+1, page.Data, err)
		}
		if index == 2 {
			if page.Data.NextCursor != nil || page.Data.BaselineToken == nil {
				t.Fatal("末页凭据不符")
			}
			break
		}
		if page.Data.NextCursor == nil || page.Data.BaselineToken != nil {
			t.Fatal("非末页凭据不符")
		}
		target := path + "/" + first.Data.SnapshotID + "?cursor=" + url.QueryEscape(*page.Data.NextCursor)
		var next dto.SyncSnapshotPageResponse
		api.request(t, ctx, http.MethodGet, target, "", nil, http.StatusOK, &next)
		page = next
	}

	// 4. 核对读页结束后设备仍待确认，且创建同键重试只保存一份回执。
	var status string
	var ack, receipts, accountSequence uint64
	if err := pool.QueryRowContext(ctx, `SELECT devices.status,devices.ack_sequence,accounts.change_seq,
		(SELECT COUNT(*) FROM idempotency_records WHERE user_id=accounts.id AND operation=?)
		FROM sync_devices AS devices JOIN users AS accounts ON accounts.id=devices.user_id WHERE devices.user_id=1`,
		domain.CreateSyncSnapshotOperation).Scan(&status, &ack, &accountSequence, &receipts); err != nil ||
		status != "RESET_REQUIRED" || ack != 0 || accountSequence != 1 || receipts != 1 {
		t.Fatalf("设备状态=%s，确认位置=%d，当前序号=%d，回执数=%d，错误=%v", status, ack, accountSequence, receipts, err)
	}

	// 5. 通过正式路由确认零序号。未授权的请求不会推进设备，同一凭据重试保持原位置和时间。
	type deviceBaselineState struct {
		status, generation string
		position           uint64
		lastSeen, updated  time.Time
	}
	readDeviceBaseline := func() deviceBaselineState {
		t.Helper()
		var state deviceBaselineState
		err := pool.QueryRowContext(ctx, `SELECT status,HEX(cursor_generation),ack_sequence,last_seen_at,updated_at
			FROM sync_devices WHERE user_id=1`).Scan(&state.status, &state.generation, &state.position, &state.lastSeen, &state.updated)
		if err != nil {
			t.Fatal("读取隔离设备基线失败", err)
		}
		return state
	}
	baselineBody := func(token string) *strings.Reader {
		return strings.NewReader(`{"baseline_token":` + strconv.Quote(token) + `}`)
	}
	firstConfirmPath := path + "/" + page.Data.SnapshotID + "/confirm"
	firstBaselineToken := *page.Data.BaselineToken
	pending := readDeviceBaseline()
	unauthorized := api
	unauthorized.accessToken = ""
	var denied dto.ErrorResponse
	unauthorized.request(t, ctx, http.MethodPost, firstConfirmPath, "", baselineBody(firstBaselineToken), http.StatusUnauthorized, &denied)
	if denied.Error.Code != "AUTH_REQUIRED" || denied.RequestID == "" || readDeviceBaseline() != pending {
		t.Fatalf("未授权确认的错误或持久状态不符：%+v", denied)
	}
	confirm := func(target, token, want string) dto.ConfirmSyncSnapshotBaselineResponse {
		t.Helper()
		var result dto.ConfirmSyncSnapshotBaselineResponse
		api.request(t, ctx, http.MethodPost, target, "", baselineBody(token), http.StatusOK, &result)
		if result.RequestID == "" || result.Data.Status != "ACTIVE" || result.Data.AckSequence != want {
			t.Fatalf("确认响应不符：%+v，期望序号 %s", result, want)
		}
		return result
	}
	confirmed := confirm(firstConfirmPath, firstBaselineToken, "0")
	active := readDeviceBaseline()
	if active.status != "ACTIVE" || active.position != 0 || active.generation != pending.generation {
		t.Fatalf("零序号确认未提交：%+v", active)
	}
	if replay := confirm(firstConfirmPath, firstBaselineToken, "0"); replay.RequestID == confirmed.RequestID || readDeviceBaseline() != active {
		t.Fatal("同一凭据重试未保留已提交的设备记录")
	}

	// 6. 从新快照确认非零序号，旧代次的凭据必须失效，并正常关闭隔离服务。
	var nextBaseline dto.SyncSnapshotPageResponse
	api.request(t, ctx, http.MethodPost, path, "startup-sync-snapshot-new-baseline", strings.NewReader(`{}`), http.StatusCreated, &nextBaseline)
	if nextBaseline.Data.SnapshotID == "" || nextBaseline.Data.SnapshotID == first.Data.SnapshotID || nextBaseline.Data.HighWaterSequence != "1" ||
		nextBaseline.Data.BaselineToken == nil || nextBaseline.Data.NextCursor != nil {
		t.Fatal("新快照未提供非零截止序号的末页凭据")
	}
	newPending := readDeviceBaseline()
	if newPending.status != "RESET_REQUIRED" || newPending.position != 0 || newPending.generation == active.generation {
		t.Fatalf("新快照未建立待确认的新代次：%+v", newPending)
	}
	var expired dto.ErrorResponse
	api.request(t, ctx, http.MethodPost, firstConfirmPath, "", baselineBody(firstBaselineToken), http.StatusGone, &expired)
	if expired.Error.Code != "CURSOR_EXPIRED" || expired.RequestID == "" || readDeviceBaseline() != newPending {
		t.Fatalf("失效凭据的错误或持久状态不符：%+v", expired)
	}
	confirm(path+"/"+nextBaseline.Data.SnapshotID+"/confirm", *nextBaseline.Data.BaselineToken, "1")
	if state := readDeviceBaseline(); state.status != "ACTIVE" || state.position != 1 || state.generation != newPending.generation {
		t.Fatalf("非零序号确认未提交：%+v", state)
	}
	if _, err := stop(); err != nil {
		t.Fatal("隔离服务未正常退出", err)
	}
	t.Logf("独占库 %s，回环服务 %s。三页保持截止序号 0，随后确认 0 并重试，新代次拒绝旧凭据且确认到 1", databaseName, api.baseURL)
}
