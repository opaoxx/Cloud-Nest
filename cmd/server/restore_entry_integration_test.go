package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

const serverRestoreDirectoryID = "74005"
const serverRestoreDirectoryKey = "server-restore-nested-directory"
const serverRestoreChildID = "74103"

func TestServerRestoreChangeEventIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_RESTORE_CHANGE_EVENT_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_RESTORE_CHANGE_EVENT_TEST=1 后启动独占 MySQL、MinIO 和服务")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	fixture := prepareBackupSource(t, ctx)
	t.Logf("恢复事件专用 MySQL %s，专用 MinIO %s", fixture.mysqlContainer[:12], fixture.minioContainer[:12])
	userID := backupFixtureID(t, fixture.login.User.ID)
	rootID := backupFixtureID(t, fixture.login.RootEntryID)
	if _, err := fixture.pool.ExecContext(ctx, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,revision,deleted_at)
		VALUES(11,?,?,'原父目录',CAST('原父目录' AS BINARY),'DIR','TRASHED',2,UTC_TIMESTAMP(6)),
		      (12,?,11,'同步恢复目录',CAST('同步恢复目录' AS BINARY),'DIR','TRASHED',3,UTC_TIMESTAMP(6)),
		      (13,?,12,'活动后代',CAST('活动后代' AS BINARY),'DIR','ACTIVE',1,NULL),
		      (14,?,?,'同步恢复目录',CAST('同步恢复目录' AS BINARY),'DIR','ACTIVE',1,NULL),
		      (15,?,?,'恢复故障目录',CAST('恢复故障目录' AS BINARY),'DIR','TRASHED',1,UTC_TIMESTAMP(6))`,
		userID, rootID, userID, userID, userID, rootID, userID, rootID); err != nil {
		t.Fatal("无法准备本轮专用回收目录树", err)
	}
	request := func(id, key, revision string, status int, code string, details map[string]any) dto.RestoreEntryResponse {
		t.Helper()
		return requestServerRestoreEntry(t, ctx, fixture.api.baseURL, fixture.api.accessToken, id, key, revision, status, code, details)
	}
	type restoreLogFacts struct{ snapshots, events, sequence uint64 }
	readLog := func() restoreLogFacts {
		t.Helper()
		var log restoreLogFacts
		if err := fixture.pool.QueryRowContext(ctx, `SELECT
			(SELECT COUNT(*) FROM idempotency_records WHERE user_id=users.id AND operation=?),
			(SELECT COUNT(*) FROM change_events WHERE user_id=users.id),change_seq FROM users WHERE id=?`,
			domain.RestoreEntryOperation, userID).Scan(&log.snapshots, &log.events, &log.sequence); err != nil {
			t.Fatal(err)
		}
		return log
	}
	checkEntry := func(id, parentID, revision uint64, status, name string) sql.NullTime {
		t.Helper()
		var savedParent, savedRevision uint64
		var savedStatus, savedName string
		var deletedAt sql.NullTime
		if err := fixture.pool.QueryRowContext(ctx, `SELECT parent_id,revision,status,name,deleted_at FROM file_entries WHERE id=? AND user_id=?`,
			id, userID).Scan(&savedParent, &savedRevision, &savedStatus, &savedName, &deletedAt); err != nil ||
			savedParent != parentID || savedRevision != revision || savedStatus != status || savedName != name || deletedAt.Valid != (status == "TRASHED") {
			t.Fatal("隔离条目的位置、名称、版本或回收事实不符", id, savedParent, savedRevision, savedStatus, savedName, deletedAt, err)
		}
		return deletedAt
	}
	checkEvent := func(sequence uint64, entry dto.RestoredEntrySummary) {
		t.Helper()
		var savedID, revision uint64
		var eventType string
		var payload []byte
		if err := fixture.pool.QueryRowContext(ctx, "SELECT entry_id,revision,event_type,payload FROM change_events WHERE user_id=? AND `sequence`=?",
			userID, sequence).Scan(&savedID, &revision, &eventType, &payload); err != nil {
			t.Fatal(err)
		}
		var fields map[string]string
		want := map[string]string{"entry_id": entry.ID, "parent_id": entry.ParentID, "name": entry.Name, "revision": entry.Revision}
		if err := json.Unmarshal(payload, &fields); err != nil || !reflect.DeepEqual(fields, want) ||
			eventType != "ENTRY_RESTORED" || strconv.FormatUint(savedID, 10) != entry.ID ||
			strconv.FormatUint(revision, 10) != entry.Revision {
			t.Fatal("恢复事件表列或载荷与实际恢复结果不符", string(payload), err)
		}
	}

	// 1. 原父目录仍在回收站，恢复时回退到根目录并避开同名条目。
	var hidden dto.ErrorResponse
	fixture.api.request(t, ctx, http.MethodGet, "/api/v1/entries/13", "", nil, http.StatusNotFound, &hidden)
	request("12", "restore-event-first", "2", http.StatusConflict, "REVISION_CONFLICT", map[string]any{"current_revision": "3"})
	if hidden.Error.Code != "ENTRY_NOT_FOUND" || readLog() != (restoreLogFacts{}) {
		t.Fatal("恢复前可见性或版本冲突的日志不符")
	}
	const firstName = "同步恢复目录 (1)"
	first := request("12", "restore-event-first", "3", http.StatusOK, "", nil)
	if first.Data.Entry != (dto.RestoredEntrySummary{
		ID: "12", ParentID: fixture.login.RootEntryID, Name: firstName, EntryType: "DIR", Status: "ACTIVE", Revision: "4",
	}) {
		t.Fatal("恢复响应未交付实际回退位置和重名后缀", first.Data)
	}
	checkEntry(12, rootID, 4, "ACTIVE", firstName)
	checkEntry(14, rootID, 1, "ACTIVE", "同步恢复目录")
	checkEvent(1, first.Data.Entry)
	var visible dto.EntryDetailResponse
	fixture.api.request(t, ctx, http.MethodGet, "/api/v1/entries/13", "", nil, http.StatusOK, &visible)
	if visible.Data.ID != "13" || visible.Data.ParentID == nil || *visible.Data.ParentID != "12" || visible.Data.Revision != "1" {
		t.Fatal("恢复根项后没有重新显示原后代", visible.Data)
	}

	// 2. 同键重放保存的实际位置，已恢复条目上的新请求保持日志。
	replay := request("12", "restore-event-first", "3", http.StatusOK, "", nil)
	request("12", "restore-event-already-active", "4", http.StatusNotFound, "ENTRY_NOT_FOUND", nil)
	afterFirst := restoreLogFacts{snapshots: 1, events: 1, sequence: 1}
	if first.Data != replay.Data || first.RequestID == replay.RequestID || readLog() != afterFirst {
		t.Fatal("同键重放或已恢复条目上的新请求改变了快照和日志")
	}

	// 3. 在状态、快照和序号写入后注入故障，核对回滚和原键重试。
	beforeFailure := checkEntry(15, rootID, 1, "TRASHED", "恢复故障目录")
	if _, err := fixture.pool.ExecContext(ctx, `CREATE TRIGGER fixture_restore_event_failure
		BEFORE INSERT ON change_events FOR EACH ROW
		BEGIN
			IF NEW.event_type='ENTRY_RESTORED'
				AND EXISTS (SELECT 1 FROM file_entries WHERE user_id=NEW.user_id AND id=NEW.entry_id
					AND id=15 AND status='ACTIVE' AND revision=2 AND deleted_at IS NULL)
				AND EXISTS (SELECT 1 FROM idempotency_records WHERE user_id=NEW.user_id AND resource_id=NEW.entry_id
					AND idempotency_key='restore-event-rollback')
				AND EXISTS (SELECT 1 FROM users WHERE id=NEW.user_id AND change_seq=2) THEN
				SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='fixture restore event insert failure';
			END IF;
		END`); err != nil {
		t.Fatal("无法安装恢复事件隔离故障", err)
	}
	request("15", "restore-event-rollback", "1", http.StatusInternalServerError, "INTERNAL_ERROR", nil)
	if deletedAt := checkEntry(15, rootID, 1, "TRASHED", "恢复故障目录"); !deletedAt.Time.Equal(beforeFailure.Time) || readLog() != afterFirst {
		t.Fatal("事件写入失败后回收时间、快照或序号未回滚")
	}
	if _, err := fixture.pool.ExecContext(ctx, "DROP TRIGGER fixture_restore_event_failure"); err != nil {
		t.Fatal(err)
	}
	recovered := request("15", "restore-event-rollback", "1", http.StatusOK, "", nil)
	checkEntry(15, rootID, 2, "ACTIVE", "恢复故障目录")
	checkEvent(1, first.Data.Entry)
	checkEvent(2, recovered.Data.Entry)
	if readLog() != (restoreLogFacts{snapshots: 2, events: 2, sequence: 2}) {
		t.Fatal("恢复故障后的原键重试未取得连续序号")
	}
	logs, err := fixture.stop()
	if err != nil {
		t.Fatal("隔离服务停机失败", err)
	}
	verifyBackupSourceStopped(t, fixture.api.baseURL, logs)
	t.Logf("目录 12 回退到根目录 %s，采用 %q、版本 4 并写事件 1。后代 13 恢复可见，冲突与重放未追加。回滚后目录 15 原键重试写事件 2",
		fixture.login.RootEntryID, firstName)
}

func requestServerRestoreEntry(t *testing.T, ctx context.Context, baseURL, token, entryID, key, revision string, status int, code string, details map[string]any) dto.RestoreEntryResponse {
	t.Helper()
	// 1. 向生产启动的服务发送正式恢复请求。
	requestID := uuid.NewString()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v1/trash/"+entryID+"/restore",
		strings.NewReader(`{"expected_revision":"`+revision+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	const origin = "http://127.0.0.1:5173"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", origin)
	request.Header.Set("X-Request-ID", strings.ToUpper(requestID))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("正式恢复请求失败", err)
	}
	defer response.Body.Close()
	headers := response.Header
	if response.StatusCode != status || headers.Get("X-Request-ID") != requestID || headers.Get("Cache-Control") != "no-store" ||
		headers.Get("Content-Type") != "application/json; charset=utf-8" || headers.Get("Access-Control-Allow-Origin") != origin ||
		(status != http.StatusUnauthorized && headers.Get("Pragma") != "no-cache") {
		t.Fatalf("正式恢复响应状态 %d，期望 %d，响应头 %v", response.StatusCode, status, headers)
	}

	// 2. 核对响应正文与当前请求 ID。
	decoder := json.NewDecoder(response.Body)
	if status == http.StatusOK {
		var result dto.RestoreEntryResponse
		if err := decoder.Decode(&result); err != nil || result.RequestID != requestID ||
			result.Data.Entry.ID != entryID || result.Data.Entry.Status != "ACTIVE" {
			t.Fatal("正式恢复成功正文不符", result, err)
		}
		return result
	}
	var failure dto.ErrorResponse
	if err := decoder.Decode(&failure); err != nil || failure.RequestID != requestID ||
		failure.Error.Code != code || !reflect.DeepEqual(failure.Error.Details, details) {
		t.Fatal("正式恢复错误正文不符", failure, err)
	}
	return dto.RestoreEntryResponse{}
}

func verifyServerRestoreEntry(t *testing.T, ctx context.Context, pool *sql.DB, baseURL, token, foreignToken string) dto.RestoreEntryResponse {
	t.Helper()
	// 1. 复用嵌套回收项，为根目录准备活动同名项，并准备原位恢复文件。
	for _, statement := range []string{
		`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type)
		  VALUES(74102,1,11,'嵌套回收项',CAST('嵌套回收项' AS BINARY),'DIR'),
		        (74103,1,74005,'恢复后可见子目录',CAST('恢复后可见子目录' AS BINARY),'DIR')`,
		`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,revision,status,deleted_at)
		  VALUES(74101,1,74001,'原位恢复.md',CAST('原位恢复.md' AS BINARY),'FILE','DOCUMENT',41,2048,3,'TRASHED','2000-01-01 00:00:00')`,
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("准备正式恢复夹具失败", err)
		}
	}

	// 2. 通过正式入口验证认证、归属、版本和两类恢复路径。
	requestServerRestoreEntry(t, ctx, baseURL, "", "0", "", "0", 401, "AUTH_REQUIRED", nil)
	requestServerRestoreEntry(t, ctx, baseURL, token, "11", "restore-root", "1", 403, "ROOT_OPERATION_FORBIDDEN", nil)
	requestServerRestoreEntry(t, ctx, baseURL, foreignToken, serverRestoreDirectoryID, serverRestoreDirectoryKey, "1", 404, "ENTRY_NOT_FOUND", nil)
	requestServerRestoreEntry(t, ctx, baseURL, token, serverRestoreDirectoryID, serverRestoreDirectoryKey, "2",
		409, "REVISION_CONFLICT", map[string]any{"current_revision": "1"})
	directory := requestServerRestoreEntry(t, ctx, baseURL, token, serverRestoreDirectoryID, serverRestoreDirectoryKey, "1", 200, "", nil)
	file := requestServerRestoreEntry(t, ctx, baseURL, token, "74101", "server-restore-file", "3", 200, "", nil)
	replay := requestServerRestoreEntry(t, ctx, baseURL, token, serverRestoreDirectoryID, serverRestoreDirectoryKey, "1", 200, "", nil)
	if directory.Data.Entry != (dto.RestoredEntrySummary{
		ID: serverRestoreDirectoryID, ParentID: "11", Name: "嵌套回收项 (1)", EntryType: "DIR", Status: "ACTIVE", Revision: "2",
	}) || file.Data.Entry != (dto.RestoredEntrySummary{
		ID: "74101", ParentID: "74001", Name: "原位恢复.md", EntryType: "FILE", Status: "ACTIVE", Revision: "4",
	}) || replay.Data != directory.Data || replay.RequestID == directory.RequestID {
		t.Fatal("正式恢复后的实际父目录、名称、版本或重试结果不符", directory, file, replay)
	}
	requestServerRestoreEntry(t, ctx, baseURL, token, serverRestoreDirectoryID, serverRestoreDirectoryKey, "2", 409, "IDEMPOTENCY_CONFLICT", nil)
	requestServerRestoreEntry(t, ctx, baseURL, token, serverRestoreDirectoryID, "restore-new-key", "2", 404, "ENTRY_NOT_FOUND", nil)

	// 3. 读回文件的持久状态和快照，验证恢复事务的两部分均已提交。
	var parentID, revision, objectID uint64
	var status string
	var deletedAt sql.NullTime
	if err := pool.QueryRowContext(ctx, "SELECT parent_id,revision,object_id,status,deleted_at FROM file_entries WHERE user_id=1 AND id=74101").
		Scan(&parentID, &revision, &objectID, &status, &deletedAt); err != nil ||
		parentID != 74001 || revision != 4 || objectID != 41 || status != "ACTIVE" || deletedAt.Valid {
		t.Fatal("正式恢复文件的数据库记录不符", err)
	}
	var stableBody []byte
	if err := pool.QueryRowContext(ctx, "SELECT response_body FROM idempotency_records WHERE user_id=1 AND operation=? AND idempotency_key=?",
		domain.RestoreEntryOperation, serverRestoreDirectoryKey).Scan(&stableBody); err != nil {
		t.Fatal("未取得正式恢复写入的快照", err)
	}
	stored, err := dto.DecodeRestoreEntryData(stableBody)
	if err != nil || stored == nil || *stored != directory.Data {
		t.Fatal("正式恢复交付与已保存快照不符", err)
	}
	return directory
}
