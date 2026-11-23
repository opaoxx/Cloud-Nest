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

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"golang.org/x/crypto/bcrypt"
)

func TestServerSyncChangePruneCursorBoundaryIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_SYNC_PRUNE_CURSOR_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_SYNC_PRUNE_CURSOR_TEST=1 后使用独占 MySQL 和回环服务")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	// 1. 建立基线，通过正式接口取得三个连续位置的游标。
	pool, databaseName := prepareServerIntegrationDatabase(t, ctx)
	password := rand.Text()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	execute("INSERT INTO users(id,username,display_name,password_hash) VALUES(1,'root','日志裁剪游标夹具',?)", string(hash))
	execute("INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR')")
	t.Setenv("CLOUDNEST_STORAGE_PROVIDER", "local_minio")
	prepareServerReadOnlyStorageStub(t, "fixture-prune-cursor")
	address := configureBackupFixtureServer(t, databaseName)
	api, stopServer := startConfiguredBackupFixtureServer(t, ctx, address, run)
	login := loginBackupFixture(t, ctx, &api, password)
	userID, deviceID := backupFixtureID(t, login.User.ID), backupFixtureID(t, login.Device.ID)
	accessToken := api.accessToken

	const snapshotPath = "/api/v1/sync/snapshots"
	const pullPath = "/api/v1/sync/changes/pull"
	const confirmPath = "/api/v1/sync/changes/confirm"
	tokenBody := func(field, token string) *strings.Reader {
		return strings.NewReader(`{"` + field + `":` + strconv.Quote(token) + `}`)
	}
	confirm := func(path, field, token, wantAck string) {
		t.Helper()
		var response dto.ConfirmSyncSnapshotBaselineResponse
		api.request(t, ctx, http.MethodPost, path, "", tokenBody(field, token), http.StatusOK, &response)
		if response.RequestID == "" || response.Data.Status != "ACTIVE" || response.Data.AckSequence != wantAck {
			t.Fatalf("确认响应不符：%+v，期望位置 %s", response, wantAck)
		}
	}
	pull := func(field, token, wantSequence, wantEntryID string, hasMore bool) string {
		t.Helper()
		var response dto.PullSyncChangesResponse
		body := strings.NewReader(`{"` + field + `":` + strconv.Quote(token) + `,"page_size":1}`)
		api.request(t, ctx, http.MethodPost, pullPath, "", body, http.StatusOK, &response)
		page := response.Data
		if response.RequestID == "" || page.Events == nil || page.HighWaterSequence != "3" || page.NextCursor == "" || page.HasMore != hasMore {
			t.Fatal("增量拉取的上界、续读游标或分页标记不符")
		}
		if wantSequence == "" {
			if len(page.Events) != 0 {
				t.Fatalf("最大序号应返回空页，实际 %d 条", len(page.Events))
			}
		} else if len(page.Events) != 1 || page.Events[0].Sequence != wantSequence ||
			page.Events[0].EntryID != wantEntryID || page.Events[0].EventType != "DIRECTORY_CREATED" {
			t.Fatalf("未交付期望的目录变更：序号 %s，条目 %s", wantSequence, wantEntryID)
		}
		return page.NextCursor
	}
	var snapshot dto.SyncSnapshotPageResponse
	api.request(t, ctx, http.MethodPost, snapshotPath, "prune-cursor-baseline", strings.NewReader(`{}`), http.StatusCreated, &snapshot)
	if snapshot.RequestID == "" || snapshot.Data.SnapshotID == "" || snapshot.Data.HighWaterSequence != "0" ||
		snapshot.Data.RootEntryID != login.RootEntryID || snapshot.Data.BaselineToken == nil || snapshot.Data.NextCursor != nil {
		t.Fatal("未取得零位置的完整基线")
	}
	confirm(snapshotPath+"/"+snapshot.Data.SnapshotID+"/confirm", "baseline_token", *snapshot.Data.BaselineToken, "0")
	var entryIDs [3]string
	for index, name := range []string{"合成目录一", "合成目录二", "合成目录三"} {
		var added dto.CreateDirectoryResponse
		body := strings.NewReader(`{"parent_id":` + strconv.Quote(login.RootEntryID) + `,"name":` + strconv.Quote(name) + `}`)
		api.request(t, ctx, http.MethodPost, "/api/v1/directories", "prune-cursor-directory-"+strconv.Itoa(index+1), body, http.StatusCreated, &added)
		entryIDs[index] = added.Data.Entry.ID
	}
	cursorOne := pull("baseline_token", *snapshot.Data.BaselineToken, "1", entryIDs[0], true)
	cursorTwo := pull("cursor", cursorOne, "2", entryIDs[1], true)
	cursorThree := pull("cursor", cursorTwo, "3", entryIDs[2], false)
	confirm(confirmPath, "cursor", cursorTwo, "2")
	confirm(confirmPath, "cursor", cursorOne, "2")

	type deviceState struct {
		status                domain.SyncDeviceStatus
		ack                   uint64
		generation            string
		lastSeenAt, updatedAt time.Time
	}
	readDevice := func() deviceState {
		t.Helper()
		var state deviceState
		err := pool.QueryRowContext(ctx, `SELECT status,ack_sequence,HEX(cursor_generation),last_seen_at,updated_at
			FROM sync_devices WHERE user_id=? AND device_id=?`, userID, deviceID).
			Scan(&state.status, &state.ack, &state.generation, &state.lastSeenAt, &state.updatedAt)
		if err != nil {
			t.Fatal("读取隔离设备同步状态失败", err)
		}
		return state
	}
	atTwo := readDevice()
	if atTwo.status != domain.SyncDeviceStatusActive || atTwo.ack != 2 || atTwo.generation == "" {
		t.Fatalf("裁剪前设备未保持活动并确认到 2：%+v", atTwo)
	}
	ageEvents := func(first, last uint64) {
		t.Helper()
		result, err := pool.ExecContext(ctx, "UPDATE change_events SET created_at=UTC_TIMESTAMP(6)-INTERVAL 91 DAY "+
			"WHERE user_id=? AND `sequence` BETWEEN ? AND ?", userID, first, last)
		if err != nil {
			t.Fatal(err)
		}
		if count, err := result.RowsAffected(); err != nil || count != int64(last-first+1) {
			t.Fatalf("回拨合成日志时间未命中预期范围：%d，%v", count, err)
		}
	}
	restart := func() string {
		t.Helper()
		restartAddress := reserveWebLoginAddress(t)
		t.Setenv("CLOUDNEST_HTTP_ADDR", restartAddress)
		api, stopServer = startConfiguredBackupFixtureServer(t, ctx, restartAddress, run)
		api.accessToken = accessToken
		return restartAddress
	}
	waitForLogWindow := func(wantCount, wantFirst uint64) {
		t.Helper()
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			var high, count, first uint64
			err := pool.QueryRowContext(ctx, "SELECT change_seq, (SELECT COUNT(*) FROM change_events WHERE user_id=users.id), "+
				"(SELECT COALESCE(MIN(`sequence`),0) FROM change_events WHERE user_id=users.id) FROM users WHERE id=?", userID).
				Scan(&high, &count, &first)
			if err != nil || high != 3 {
				t.Fatalf("裁剪后的最大序号不符：%d，%v", high, err)
			}
			if count == wantCount && first == wantFirst {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatalf("后台未到达预期日志范围：首条 %d，条数 %d", first, count)
			case <-ticker.C:
			}
		}
	}
	assertExpired := func(token string) {
		t.Helper()
		before := readDevice()
		for _, path := range []string{pullPath, confirmPath} {
			var failure dto.ErrorResponse
			api.request(t, ctx, http.MethodPost, path, "", tokenBody("cursor", token), http.StatusGone, &failure)
			if failure.RequestID == "" || failure.Error.Code != "CURSOR_EXPIRED" || readDevice() != before {
				t.Fatalf("越过日志下界后的错误或设备状态不符：%s，%+v", path, failure)
			}
		}
	}
	stopAndCheckPruned := func(deleted string) {
		t.Helper()
		logs, err := stopServer()
		if err != nil || !strings.Contains(logs, "同步日志裁剪轮次结束") ||
			!strings.Contains(logs, `"deleted_events":`+deleted+`,`) {
			t.Fatalf("裁剪轮次汇报缺少预期删除数 %s，停止错误 %v", deleted, err)
		}
	}

	// 2. 让后台裁剪前两条日志，验证旧游标失效、边界游标仍可续读。
	if _, err := stopServer(); err != nil {
		t.Fatal(err)
	}
	ageEvents(1, 2)
	partialAddress := restart()
	waitForLogWindow(1, 3)
	if afterPrune := readDevice(); afterPrune != atTwo {
		t.Fatalf("部分裁剪改变了设备状态：%+v，裁剪前 %+v", afterPrune, atTwo)
	}
	assertExpired(cursorOne)
	confirm(confirmPath, "cursor", cursorTwo, "2")
	resumed := pull("cursor", cursorTwo, "3", entryIDs[2], false)
	if emptyCursor := pull("cursor", resumed, "", "", false); emptyCursor != resumed {
		t.Fatal("最大序号的空页改变了续读位置")
	}
	confirm(confirmPath, "cursor", resumed, "3")
	atThree := readDevice()
	if atThree.ack != 3 || atThree.status != atTwo.status || atThree.generation != atTwo.generation {
		t.Fatalf("续读确认未推进到 3 或改变了代次：%+v", atThree)
	}
	stopAndCheckPruned("2")

	// 3. 确认最后一条后裁剪剩余日志，验证最大序号仍可读取空页。
	ageEvents(3, 3)
	emptyAddress := restart()
	waitForLogWindow(0, 0)
	if afterPrune := readDevice(); afterPrune != atThree {
		t.Fatalf("清空日志改变了设备状态：%+v，裁剪前 %+v", afterPrune, atThree)
	}
	assertExpired(cursorTwo)
	if emptyCursor := pull("cursor", cursorThree, "", "", false); emptyCursor != cursorThree {
		t.Fatal("日志清空后最大序号的续读位置发生变化")
	}
	confirm(confirmPath, "cursor", cursorThree, "3")
	var retainedDirectories uint64
	err = pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM file_entries WHERE user_id=? AND parent_id=11 AND status='ACTIVE'", userID).
		Scan(&retainedDirectories)
	if err != nil || retainedDirectories != 3 {
		t.Fatalf("裁剪没有保留三条业务目录：%d，%v", retainedDirectories, err)
	}
	stopAndCheckPruned("1")
	t.Logf("独占库 %s，回环服务 %s → %s → %s。同代次下界从 0 到 2 再到 3，前一位拉取和确认均返回 410，等值正常，最大序号保持 3",
		databaseName, address, partialAddress, emptyAddress)
}
