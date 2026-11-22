package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"reflect"
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

	// 4. 确认设备仍待确认、只保存一份创建回执，并正常关闭隔离服务。
	var status string
	var ack, receipts, accountSequence uint64
	if err := pool.QueryRowContext(ctx, `SELECT devices.status,devices.ack_sequence,accounts.change_seq,
		(SELECT COUNT(*) FROM idempotency_records WHERE user_id=accounts.id AND operation=?)
		FROM sync_devices AS devices JOIN users AS accounts ON accounts.id=devices.user_id WHERE devices.user_id=1`,
		domain.CreateSyncSnapshotOperation).Scan(&status, &ack, &accountSequence, &receipts); err != nil ||
		status != "RESET_REQUIRED" || ack != 0 || accountSequence != 1 || receipts != 1 {
		t.Fatalf("设备状态=%s，确认位置=%d，当前序号=%d，回执数=%d，错误=%v", status, ack, accountSequence, receipts, err)
	}
	if _, err := stop(); err != nil {
		t.Fatal("隔离服务未正常退出", err)
	}
	t.Logf("独占库 %s，回环服务 %s。三页均为截止序号 0，目录写入后账号序号为 1", databaseName, api.baseURL)
}
