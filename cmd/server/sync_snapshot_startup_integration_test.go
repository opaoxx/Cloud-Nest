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
	const pullPath = "/api/v1/sync/changes/pull"
	pullBody := func(field, token string) *strings.Reader {
		return strings.NewReader(`{"` + field + `":` + strconv.Quote(token) + `,"page_size":1}`)
	}
	pending := readDeviceBaseline()
	unauthorized := api
	unauthorized.accessToken = ""
	var denied dto.ErrorResponse
	unauthorized.request(t, ctx, http.MethodPost, firstConfirmPath, "", baselineBody(firstBaselineToken), http.StatusUnauthorized, &denied)
	if denied.Error.Code != "AUTH_REQUIRED" || denied.RequestID == "" || readDeviceBaseline() != pending {
		t.Fatalf("未授权确认的错误或持久状态不符：%+v", denied)
	}
	unauthorized.request(t, ctx, http.MethodPost, pullPath, "", pullBody("baseline_token", firstBaselineToken), http.StatusUnauthorized, &denied)
	if denied.Error.Code != "AUTH_REQUIRED" || denied.RequestID == "" || readDeviceBaseline() != pending {
		t.Fatal("未授权拉取应被拒绝且设备记录保持原值")
	}
	var pendingPull dto.ErrorResponse
	api.request(t, ctx, http.MethodPost, pullPath, "", pullBody("baseline_token", firstBaselineToken), http.StatusGone, &pendingPull)
	if pendingPull.Error.Code != "CURSOR_EXPIRED" || readDeviceBaseline() != pending {
		t.Fatal("未确认的快照凭据应失效且设备记录保持原值")
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

	// 6. 再创建一条合成目录，从确认的基线拉取、续读并轮询空页。
	var later dto.CreateDirectoryResponse
	api.request(t, ctx, http.MethodPost, "/api/v1/directories", "startup-sync-fixture-second-directory",
		strings.NewReader(`{"parent_id":"11","name":"确认之后创建"}`), http.StatusCreated, &later)
	pull := func(field, token string) dto.PullSyncChangesResponse {
		t.Helper()
		var result dto.PullSyncChangesResponse
		api.request(t, ctx, http.MethodPost, pullPath, "", pullBody(field, token), http.StatusOK, &result)
		if result.RequestID == "" || result.Data.Events == nil || result.Data.HighWaterSequence != "2" || result.Data.NextCursor == "" {
			t.Fatal("增量页缺少有效的上界、请求 ID、数组或续读游标")
		}
		return result
	}
	assertDirectoryEvent := func(page dto.PullSyncChangesResponse, sequence string, entry dto.CreatedDirectorySummary, hasMore bool) {
		t.Helper()
		if len(page.Data.Events) != 1 || page.Data.HasMore != hasMore {
			t.Fatalf("增量页序号 %s 的条数或后续标记不符", sequence)
		}
		event := page.Data.Events[0]
		var payload map[string]string
		wantPayload := map[string]string{"entry_id": entry.ID, "parent_id": entry.ParentID, "name": entry.Name, "revision": entry.Revision}
		if err := json.Unmarshal(event.Payload, &payload); err != nil || event.Sequence != sequence ||
			event.EntryID != entry.ID || event.Revision != entry.Revision || event.EventType != "DIRECTORY_CREATED" ||
			!reflect.DeepEqual(payload, wantPayload) {
			t.Fatalf("拉取的目录事件和已提交目录不符，期望序号 %s，错误 %v", sequence, err)
		}
	}
	firstChanges := pull("baseline_token", firstBaselineToken)
	assertDirectoryEvent(firstChanges, "1", added.Data.Entry, true)
	again := pull("baseline_token", firstBaselineToken)
	if again.RequestID == firstChanges.RequestID || !reflect.DeepEqual(again.Data, firstChanges.Data) {
		t.Fatal("重复拉取应交付同一页与新请求 ID")
	}
	lastChanges := pull("cursor", firstChanges.Data.NextCursor)
	assertDirectoryEvent(lastChanges, "2", later.Data.Entry, false)
	empty := pull("cursor", lastChanges.Data.NextCursor)
	if len(empty.Data.Events) != 0 || empty.Data.HasMore || empty.Data.NextCursor != lastChanges.Data.NextCursor || readDeviceBaseline() != active {
		t.Fatal("空页应保留续读位置，拉取过程应保持设备状态、确认位置和时间不变")
	}

	// 7. 通过正式接口确认拉取游标，核对推进、重复、较旧确认和日志边界。
	const changeConfirmPath = "/api/v1/sync/changes/confirm"
	changeConfirmBody := func(cursor string) *strings.Reader {
		return strings.NewReader(`{"cursor":` + strconv.Quote(cursor) + `}`)
	}
	unauthorized.request(t, ctx, http.MethodPost, changeConfirmPath, "", changeConfirmBody(firstChanges.Data.NextCursor), http.StatusUnauthorized, &denied)
	if denied.Error.Code != "AUTH_REQUIRED" || denied.RequestID == "" || readDeviceBaseline() != active {
		t.Fatal("未授权的增量确认应返回 401 且保持设备记录")
	}

	confirmChange := func(cursor, want string) dto.ConfirmSyncSnapshotBaselineResponse {
		t.Helper()
		var result dto.ConfirmSyncSnapshotBaselineResponse
		api.request(t, ctx, http.MethodPost, changeConfirmPath, "", changeConfirmBody(cursor), http.StatusOK, &result)
		if result.RequestID == "" || result.Data.Status != "ACTIVE" || result.Data.AckSequence != want {
			t.Fatalf("增量确认响应不符：%+v，期望序号 %s", result, want)
		}
		return result
	}
	firstAck := confirmChange(firstChanges.Data.NextCursor, "1")
	atOne := readDeviceBaseline()
	if atOne.status != "ACTIVE" || atOne.position != 1 || atOne.generation != active.generation ||
		atOne.lastSeen.Before(active.lastSeen) || !atOne.lastSeen.Equal(atOne.updated) {
		t.Fatalf("增量确认未正确提交位置和同步活动时间：%+v", atOne)
	}
	repeatedAck := confirmChange(firstChanges.Data.NextCursor, "1")
	repeatedAtOne := readDeviceBaseline()
	if repeatedAck.RequestID == firstAck.RequestID || repeatedAtOne.position != 1 ||
		repeatedAtOne.generation != atOne.generation || repeatedAtOne.status != atOne.status ||
		repeatedAtOne.lastSeen.Before(atOne.lastSeen) || !repeatedAtOne.lastSeen.Equal(repeatedAtOne.updated) {
		t.Fatalf("等值确认未保持位置并记录活动：%+v", repeatedAtOne)
	}
	confirmChange(lastChanges.Data.NextCursor, "2")
	atTwo := readDeviceBaseline()
	if atTwo.position != 2 || atTwo.generation != active.generation || atTwo.status != "ACTIVE" ||
		atTwo.lastSeen.Before(repeatedAtOne.lastSeen) || !atTwo.lastSeen.Equal(atTwo.updated) {
		t.Fatalf("第二页的确认位置或活动时间不符：%+v", atTwo)
	}
	confirmChange(firstChanges.Data.NextCursor, "2")
	if readDeviceBaseline() != atTwo {
		t.Fatal("较旧的有效确认应返回当前位置并保持设备行")
	}
	var events uint64
	if err := pool.QueryRowContext(ctx, `SELECT change_seq, (SELECT COUNT(*) FROM change_events WHERE user_id=1)
		FROM users WHERE id=1`).Scan(&accountSequence, &events); err != nil || accountSequence != 2 || events != 2 {
		t.Fatalf("确认后账号上界或事件条数不符：上界 %d，事件 %d，错误 %v", accountSequence, events, err)
	}
	t.Log("实际拉取游标确认到 1 和 2，重复确认保留 1，较旧游标返回 2 且设备行保持原样，变更事件仍为 2 条")

	// 8. 从新快照确认非零序号，旧代次的确认和拉取凭据必须失效。
	var nextBaseline dto.SyncSnapshotPageResponse
	api.request(t, ctx, http.MethodPost, path, "startup-sync-snapshot-new-baseline", strings.NewReader(`{}`), http.StatusCreated, &nextBaseline)
	if nextBaseline.Data.SnapshotID == "" || nextBaseline.Data.SnapshotID == first.Data.SnapshotID || nextBaseline.Data.HighWaterSequence != "2" ||
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
	api.request(t, ctx, http.MethodPost, changeConfirmPath, "", changeConfirmBody(lastChanges.Data.NextCursor), http.StatusGone, &expired)
	if expired.Error.Code != "CURSOR_EXPIRED" || expired.RequestID == "" || readDeviceBaseline() != newPending {
		t.Fatal("待确认的新代次应拒绝旧的增量确认且保持设备记录")
	}
	confirm(path+"/"+nextBaseline.Data.SnapshotID+"/confirm", *nextBaseline.Data.BaselineToken, "2")
	if state := readDeviceBaseline(); state.status != "ACTIVE" || state.position != 2 || state.generation != newPending.generation {
		t.Fatalf("非零序号确认未提交：%+v", state)
	}
	newActive := readDeviceBaseline()
	var staleCursor dto.ErrorResponse
	api.request(t, ctx, http.MethodPost, pullPath, "", pullBody("cursor", lastChanges.Data.NextCursor), http.StatusGone, &staleCursor)
	if staleCursor.Error.Code != "CURSOR_EXPIRED" || staleCursor.RequestID == "" || readDeviceBaseline() != newActive {
		t.Fatal("新代次生效后应拒绝旧代次的增量游标且保持设备记录")
	}
	var staleConfirmation dto.ErrorResponse
	api.request(t, ctx, http.MethodPost, changeConfirmPath, "", changeConfirmBody(lastChanges.Data.NextCursor), http.StatusGone, &staleConfirmation)
	if staleConfirmation.Error.Code != "CURSOR_EXPIRED" || staleConfirmation.RequestID == "" || readDeviceBaseline() != newActive {
		t.Fatal("新代次生效后应拒绝旧的增量确认且保持设备记录")
	}
	if _, err := stop(); err != nil {
		t.Fatal("隔离服务未正常退出", err)
	}
	t.Logf("独占库 %s，回环服务 %s。确认基线 0，拉取 1、2 和空页，增量确认落库至 2，新代次前后拒绝旧确认", databaseName, api.baseURL)
}
