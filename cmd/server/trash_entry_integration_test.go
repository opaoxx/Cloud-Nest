package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
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

const serverTrashDirectoryID = "74002"
const serverTrashDirectoryKey = "server-trash-directory"
const serverTrashDirectoryBody = `{"expected_revision":"1"}`

func TestServerTrashChangeEventIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_TRASH_CHANGE_EVENT_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_TRASH_CHANGE_EVENT_TEST=1 后启动独占 MySQL、MinIO 和服务")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	fixture := prepareBackupSource(t, ctx)
	t.Logf("回收事件专用 MySQL %s，专用 MinIO %s", fixture.mysqlContainer[:12], fixture.minioContainer[:12])
	userID := backupFixtureID(t, fixture.login.User.ID)
	rootID := backupFixtureID(t, fixture.login.RootEntryID)
	if _, err := fixture.pool.ExecContext(ctx, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type)
		VALUES(12,?,?,'待回收目录',CAST('待回收目录' AS BINARY),'DIR'),
		      (13,?,12,'活动后代',CAST('活动后代' AS BINARY),'DIR'),
		      (14,?,?,'回收故障目录',CAST('回收故障目录' AS BINARY),'DIR')`,
		userID, rootID, userID, userID, rootID); err != nil {
		t.Fatal("无法准备本轮专用目录树", err)
	}
	request := func(id, key, body string, status int, code string, details map[string]any) dto.TrashEntryResponse {
		t.Helper()
		return requestServerTrashEntry(t, ctx, fixture.api.baseURL, fixture.api.accessToken, id, key, body, status, code, details)
	}
	type trashLogFacts struct{ snapshots, events, sequence uint64 }
	readLog := func() trashLogFacts {
		t.Helper()
		var log trashLogFacts
		if err := fixture.pool.QueryRowContext(ctx, `SELECT
			(SELECT COUNT(*) FROM idempotency_records WHERE user_id=users.id AND operation=?),
			(SELECT COUNT(*) FROM change_events WHERE user_id=users.id),change_seq FROM users WHERE id=?`,
			domain.TrashEntryOperation, userID).Scan(&log.snapshots, &log.events, &log.sequence); err != nil {
			t.Fatal(err)
		}
		return log
	}
	checkEntry := func(id, parentID, revision uint64, status string, deletedAt time.Time) {
		t.Helper()
		var savedParent, savedRevision uint64
		var savedStatus string
		var savedDeletedAt sql.NullTime
		if err := fixture.pool.QueryRowContext(ctx, `SELECT parent_id,revision,status,deleted_at FROM file_entries WHERE id=? AND user_id=?`,
			id, userID).Scan(&savedParent, &savedRevision, &savedStatus, &savedDeletedAt); err != nil ||
			savedParent != parentID || savedRevision != revision || savedStatus != status ||
			savedDeletedAt.Valid != !deletedAt.IsZero() || !savedDeletedAt.Time.Equal(deletedAt) {
			t.Fatal("隔离条目的父目录、版本、状态或回收时间不符", id, savedParent, savedRevision, savedStatus, savedDeletedAt, err)
		}
	}
	checkEvent := func(sequence uint64, entry dto.TrashedEntrySummary) {
		t.Helper()
		var savedID, revision uint64
		var eventType string
		var payload []byte
		if err := fixture.pool.QueryRowContext(ctx, "SELECT entry_id,revision,event_type,payload FROM change_events WHERE user_id=? AND `sequence`=?",
			userID, sequence).Scan(&savedID, &revision, &eventType, &payload); err != nil {
			t.Fatal(err)
		}
		var fields map[string]string
		want := map[string]string{"entry_id": entry.ID, "parent_id": fixture.login.RootEntryID, "name": entry.Name, "revision": entry.Revision}
		if err := json.Unmarshal(payload, &fields); err != nil || !reflect.DeepEqual(fields, want) ||
			eventType != "ENTRY_TRASHED" || strconv.FormatUint(savedID, 10) != entry.ID ||
			strconv.FormatUint(revision, 10) != entry.Revision || entry.ParentID != fixture.login.RootEntryID {
			t.Fatal("回收事件表列或载荷与所存条目不符", string(payload), err)
		}
	}
	const body = `{"expected_revision":"1"}`

	// 1. 校验旧版本被拒，再回收根项，核对活动后代的继承可见性。
	var visible dto.EntryDetailResponse
	fixture.api.request(t, ctx, http.MethodGet, "/api/v1/entries/13", "", nil, http.StatusOK, &visible)
	request("12", "trash-event-first", `{"expected_revision":"2"}`, http.StatusConflict, "REVISION_CONFLICT", map[string]any{"current_revision": "1"})
	if visible.Data.ID != "13" || readLog() != (trashLogFacts{}) {
		t.Fatal("回收前可见性或版本冲突的日志不符")
	}
	first := request("12", "trash-event-first", body, http.StatusOK, "", nil)
	checkEntry(12, rootID, 2, "TRASHED", first.Data.Entry.DeletedAt)
	checkEntry(13, 12, 1, "ACTIVE", time.Time{})
	checkEvent(1, first.Data.Entry)
	var hidden dto.ErrorResponse
	fixture.api.request(t, ctx, http.MethodGet, "/api/v1/entries/13", "", nil, http.StatusNotFound, &hidden)
	if hidden.Error.Code != "ENTRY_NOT_FOUND" {
		t.Fatal("回收根项后仍能从活动入口读取后代", hidden.Error.Code)
	}

	// 2. 同键重放返回首份快照，隐藏项上的新请求保留原账目。
	replay := request("12", "trash-event-first", body, http.StatusOK, "", nil)
	request("12", "trash-event-already-hidden", `{"expected_revision":"2"}`, http.StatusNotFound, "ENTRY_NOT_FOUND", nil)
	request("13", "trash-event-hidden-child", body, http.StatusNotFound, "ENTRY_NOT_FOUND", nil)
	afterFirst := trashLogFacts{snapshots: 1, events: 1, sequence: 1}
	if !reflect.DeepEqual(first.Data, replay.Data) || first.RequestID == replay.RequestID || readLog() != afterFirst {
		t.Fatal("同键重放或隐藏项请求改变已提交的快照和日志")
	}

	// 3. 在回收状态、快照和序号写入后注入故障，验证回滚和原键重试。
	if _, err := fixture.pool.ExecContext(ctx, `CREATE TRIGGER fixture_trash_event_failure
		BEFORE INSERT ON change_events FOR EACH ROW
		BEGIN
			IF NEW.event_type='ENTRY_TRASHED'
				AND EXISTS (SELECT 1 FROM file_entries WHERE user_id=NEW.user_id AND id=NEW.entry_id
					AND id=14 AND status='TRASHED' AND revision=2 AND deleted_at IS NOT NULL)
				AND EXISTS (SELECT 1 FROM idempotency_records WHERE user_id=NEW.user_id AND resource_id=NEW.entry_id
					AND idempotency_key='trash-event-rollback')
				AND EXISTS (SELECT 1 FROM users WHERE id=NEW.user_id AND change_seq=2) THEN
				SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='fixture trash event insert failure';
			END IF;
		END`); err != nil {
		t.Fatal("无法安装回收事件隔离故障", err)
	}
	request("14", "trash-event-rollback", body, http.StatusInternalServerError, "INTERNAL_ERROR", nil)
	checkEntry(14, rootID, 1, "ACTIVE", time.Time{})
	if readLog() != afterFirst {
		t.Fatal("写入故障后回收快照或序号未回滚")
	}
	if _, err := fixture.pool.ExecContext(ctx, "DROP TRIGGER fixture_trash_event_failure"); err != nil {
		t.Fatal(err)
	}
	recovered := request("14", "trash-event-rollback", body, http.StatusOK, "", nil)
	checkEntry(14, rootID, 2, "TRASHED", recovered.Data.Entry.DeletedAt)
	checkEvent(1, first.Data.Entry)
	checkEvent(2, recovered.Data.Entry)
	if readLog() != (trashLogFacts{snapshots: 2, events: 2, sequence: 2}) {
		t.Fatal("原键重试未取得连续事件序号")
	}
	logs, err := fixture.stop()
	if err != nil {
		t.Fatal("隔离服务停机失败", err)
	}
	verifyBackupSourceStopped(t, fixture.api.baseURL, logs)
	t.Log("目录 12 回收写事件 1，后代 13 保留 ACTIVE 及原版本，活动读取返回 404。重放与拒绝请求未追加事件，故障回滚后重试目录 14 写事件 2")
}

// requestServerTrashEntry 核对生产启动后正式回收路径的 HTTP 交付。
func requestServerTrashEntry(t *testing.T, ctx context.Context, baseURL, token, id, key, body string, status int, code string, details map[string]any) dto.TrashEntryResponse {
	t.Helper()
	// 1. 发送真实 POST，并为本次请求分配独立 ID。
	requestID := uuid.NewString()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v1/entries/"+id+"/trash", strings.NewReader(body))
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
		t.Fatal("正式回收请求失败", err)
	}
	encoded, readError := io.ReadAll(response.Body)
	closeError := response.Body.Close()
	if readError != nil || closeError != nil || response.StatusCode != status ||
		response.Header.Get("X-Request-ID") != requestID || response.Header.Get("Cache-Control") != "no-store" ||
		response.Header.Get("Content-Type") != "application/json; charset=utf-8" ||
		response.Header.Get("Access-Control-Allow-Origin") != origin ||
		(status != http.StatusUnauthorized && response.Header.Get("Pragma") != "no-cache") {
		t.Fatal("正式回收状态或响应头不符", response.StatusCode, status, readError, closeError)
	}

	// 2. 核对公开响应及请求关联。
	var result dto.TrashEntryResponse
	if status == http.StatusOK {
		if json.Unmarshal(encoded, &result) != nil || result.RequestID != requestID || result.Data.Entry.ID != id ||
			result.Data.Entry.Status != "TRASHED" {
			t.Fatal("正式回收成功响应不符")
		}
		return result
	}
	var failure dto.ErrorResponse
	if json.Unmarshal(encoded, &failure) != nil || failure.RequestID != requestID ||
		failure.Error.Code != code || !reflect.DeepEqual(failure.Error.Details, details) {
		t.Fatal("正式回收错误响应不符", failure.Error)
	}
	return result
}

func verifyServerEntryTrash(t *testing.T, ctx context.Context, pool *sql.DB, baseURL, token, foreignToken string) dto.TrashEntryResponse {
	t.Helper()
	// 1. 在启动联测的独占库中准备文件、目录及活动后代。
	for _, statement := range []string{
		`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type)
		 VALUES(74001,1,11,'回收测试目录',CAST('回收测试目录' AS BINARY),'DIR'),
		       (74002,1,74001,'待回收目录',CAST('待回收目录' AS BINARY),'DIR')`,
		`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,revision)
		 VALUES(74003,1,74002,'活动后代.md',CAST('活动后代.md' AS BINARY),'FILE','DOCUMENT',41,2048,1),
		       (74004,1,74001,'待回收文件.md',CAST('待回收文件.md' AS BINARY),'FILE','DOCUMENT',41,2048,3)`,
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("准备正式回收夹具失败", err)
		}
	}
	requestServerTrashEntry(t, ctx, baseURL, "", "0", "", serverTrashDirectoryBody, 401, "AUTH_REQUIRED", nil)
	requestServerTrashEntry(t, ctx, baseURL, token, "11", "trash-root", serverTrashDirectoryBody, 403, "ROOT_OPERATION_FORBIDDEN", nil)
	requestServerTrashEntry(t, ctx, baseURL, foreignToken, serverTrashDirectoryID, serverTrashDirectoryKey, serverTrashDirectoryBody, 404, "ENTRY_NOT_FOUND", nil)
	requestServerTrashEntry(t, ctx, baseURL, token, "74004", "trash-file", serverTrashDirectoryBody, 409, "REVISION_CONFLICT", map[string]any{"current_revision": "3"})

	// 2. 通过生产路由回收文件和目录，同键同参返回首份结果。
	file := requestServerTrashEntry(t, ctx, baseURL, token, "74004", "trash-file", `{"expected_revision":"3"}`, 200, "", nil)
	first := requestServerTrashEntry(t, ctx, baseURL, token, serverTrashDirectoryID, serverTrashDirectoryKey, serverTrashDirectoryBody, 200, "", nil)
	replayed := requestServerTrashEntry(t, ctx, baseURL, token, serverTrashDirectoryID, serverTrashDirectoryKey, serverTrashDirectoryBody, 200, "", nil)
	if file.Data.Entry.EntryType != "FILE" || file.Data.Entry.Revision != "4" || file.Data.Entry.ParentID != "74001" ||
		first.Data.Entry.EntryType != "DIR" || first.Data.Entry.Revision != "2" || first.Data.Entry.ParentID != "74001" ||
		!reflect.DeepEqual(first.Data, replayed.Data) || first.RequestID == replayed.RequestID {
		t.Fatal("正式文件、目录或同键回收快照不符")
	}
	requestServerTrashEntry(t, ctx, baseURL, token, serverTrashDirectoryID, serverTrashDirectoryKey, `{"expected_revision":"2"}`, 409, "IDEMPOTENCY_CONFLICT", nil)
	requestServerTrashEntry(t, ctx, baseURL, token, "74003", "trash-hidden-child", serverTrashDirectoryBody, 404, "ENTRY_NOT_FOUND", nil)

	// 3. 核对目录项、后代和稳定结果的持久化事实。
	var status string
	var revision, objectID uint64
	var deletedAt sql.NullTime
	if err := pool.QueryRowContext(ctx, "SELECT status,revision,object_id,deleted_at FROM file_entries WHERE user_id=1 AND id=74003").
		Scan(&status, &revision, &objectID, &deletedAt); err != nil || status != "ACTIVE" || revision != 1 || objectID != 41 || deletedAt.Valid {
		t.Fatal("回收目录时改变了活动后代", err)
	}
	if err := pool.QueryRowContext(ctx, "SELECT status,revision,object_id,deleted_at FROM file_entries WHERE user_id=1 AND id=74004").
		Scan(&status, &revision, &objectID, &deletedAt); err != nil || status != "TRASHED" || revision != 4 ||
		objectID != 41 || !deletedAt.Valid || !deletedAt.Time.Equal(file.Data.Entry.DeletedAt) {
		t.Fatal("正式回收文件的状态、版本、对象或时间不符", err)
	}
	var directoryDeletedAt time.Time
	if err := pool.QueryRowContext(ctx, "SELECT status,revision,deleted_at FROM file_entries WHERE user_id=1 AND id=74002").
		Scan(&status, &revision, &directoryDeletedAt); err != nil || status != "TRASHED" || revision != 2 ||
		!directoryDeletedAt.Equal(first.Data.Entry.DeletedAt) {
		t.Fatal("正式回收目录的状态、版本或时间不符", err)
	}
	var stableBody []byte
	if err := pool.QueryRowContext(ctx, "SELECT response_body FROM idempotency_records WHERE user_id=1 AND operation=? AND idempotency_key=?",
		domain.TrashEntryOperation, serverTrashDirectoryKey).Scan(&stableBody); err != nil {
		t.Fatal("未取得正式回收写入的幂等结果", err)
	}
	stored, err := dto.RestoreTrashEntryData(stableBody)
	if err != nil || !reflect.DeepEqual(stored, &first.Data) {
		t.Fatal("正式回收交付与已保存快照不符", err)
	}
	return first
}
