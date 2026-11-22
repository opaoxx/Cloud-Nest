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

const serverMoveEntryBody = `{"target_parent_id":"70003","expected_revision":"1"}`
const serverMoveEntryKey = "server-move-007"

func TestServerMoveChangeEventIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MOVE_CHANGE_EVENT_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_MOVE_CHANGE_EVENT_TEST=1 后启动独占 MySQL、MinIO 和服务")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	fixture := prepareBackupSource(t, ctx)
	t.Logf("移动事件专用 MySQL %s，专用 MinIO %s", fixture.mysqlContainer[:12], fixture.minioContainer[:12])
	userID := backupFixtureID(t, fixture.login.User.ID)
	rootID := backupFixtureID(t, fixture.login.RootEntryID)
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type)
			VALUES(11,?,?,'源目录',CAST('源目录' AS BINARY),'DIR'),(12,?,?,'目标目录',CAST('目标目录' AS BINARY),'DIR')`, []any{userID, rootID, userID, rootID}},
		{`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type)
			VALUES(14,?,11,'待移动目录',CAST('待移动目录' AS BINARY),'DIR'),(15,?,14,'后代目录',CAST('后代目录' AS BINARY),'DIR')`, []any{userID, userID}},
	} {
		if _, err := fixture.pool.ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatal("准备隔离目录树失败", err)
		}
	}

	request := func(key, body string, status int, code string) dto.MoveEntryResponse {
		t.Helper()
		return requestServerMoveEntry(t, fixture.api.baseURL, fixture.api.accessToken, "14", key, body, status, code, "")
	}
	type moveFacts struct {
		parentID, revision, childParentID, childRevision, snapshots, events, sequence uint64
	}
	readFacts := func() moveFacts {
		t.Helper()
		var facts moveFacts
		if err := fixture.pool.QueryRowContext(ctx, `SELECT e.parent_id,e.revision,c.parent_id,c.revision,
			(SELECT COUNT(*) FROM idempotency_records WHERE user_id=u.id AND operation=?),
			(SELECT COUNT(*) FROM change_events WHERE user_id=u.id),u.change_seq
			FROM users u JOIN file_entries e ON e.user_id=u.id JOIN file_entries c ON c.user_id=u.id
			WHERE u.id=? AND e.id=14 AND c.id=15`, domain.MoveEntryOperation, userID).
			Scan(&facts.parentID, &facts.revision, &facts.childParentID, &facts.childRevision, &facts.snapshots, &facts.events, &facts.sequence); err != nil {
			t.Fatal(err)
		}
		return facts
	}
	verifyEvent := func(sequence uint64, entry dto.EntryDetailData, parentID string) {
		t.Helper()
		var entryID, revision uint64
		var eventType string
		var payload []byte
		if err := fixture.pool.QueryRowContext(ctx, "SELECT entry_id,revision,event_type,payload FROM change_events WHERE user_id=? AND `sequence`=?",
			userID, sequence).Scan(&entryID, &revision, &eventType, &payload); err != nil {
			t.Fatal(err)
		}
		var fields map[string]string
		want := map[string]string{"entry_id": "14", "parent_id": parentID, "name": "待移动目录", "revision": entry.Revision}
		if err := json.Unmarshal(payload, &fields); err != nil || !reflect.DeepEqual(fields, want) ||
			eventType != "ENTRY_MOVED" || entryID != 14 || strconv.FormatUint(revision, 10) != entry.Revision ||
			entry.ID != "14" || entry.ParentID == nil || *entry.ParentID != parentID || entry.Name != "待移动目录" {
			t.Fatal("移动事件表列或载荷和事务结果不一致", string(payload), err)
		}
	}

	// 1. 实际移动只写被移动的目录，后代仍引用原父项。
	initial := moveFacts{parentID: 11, revision: 1, childParentID: 14, childRevision: 1}
	if readFacts() != initial {
		t.Fatal("合成目录树初始状态不符")
	}
	const moveBody = `{"target_parent_id":"12","expected_revision":"1"}`
	first := request("move-event-first", moveBody, http.StatusOK, "")
	afterMove := moveFacts{parentID: 12, revision: 2, childParentID: 14, childRevision: 1, snapshots: 1, events: 1, sequence: 1}
	if first.Data.Entry.Revision != "2" || readFacts() != afterMove {
		t.Fatal("实际移动未保存预期父目录、版本和单条事件")
	}
	verifyEvent(1, first.Data.Entry, "12")

	// 2. 同键重放、版本冲突和同位置请求保持原日志。
	replay := request("move-event-first", moveBody, http.StatusOK, "")
	request("move-event-stale", `{"target_parent_id":"11","expected_revision":"1"}`, http.StatusConflict, "REVISION_CONFLICT")
	if !reflect.DeepEqual(replay.Data, first.Data) || replay.RequestID == first.RequestID || readFacts() != afterMove {
		t.Fatal("重放或版本冲突改变了已提交事实")
	}
	samePosition := request("move-event-same-position", `{"target_parent_id":"12","expected_revision":"2"}`, http.StatusOK, "")
	beforeFailure := afterMove
	beforeFailure.snapshots++
	if !reflect.DeepEqual(samePosition.Data, first.Data) || readFacts() != beforeFailure {
		t.Fatal("移到当前父目录改变了条目或日志")
	}

	// 3. 在移动、幂等快照和序号写入后注入事件故障，验证回滚与原键重试。
	if _, err := fixture.pool.ExecContext(ctx, `CREATE TRIGGER fixture_move_event_failure
		BEFORE INSERT ON change_events FOR EACH ROW
		BEGIN
			IF NEW.event_type='ENTRY_MOVED'
				AND EXISTS (SELECT 1 FROM file_entries WHERE user_id=NEW.user_id AND id=NEW.entry_id AND parent_id=11 AND revision=3)
				AND EXISTS (SELECT 1 FROM idempotency_records WHERE user_id=NEW.user_id AND resource_id=NEW.entry_id
					AND idempotency_key='move-event-rollback')
				AND EXISTS (SELECT 1 FROM users WHERE id=NEW.user_id AND change_seq=2) THEN
				SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='fixture move event insert failure';
			END IF;
		END`); err != nil {
		t.Fatal("无法安装移动事件隔离故障", err)
	}
	const backBody = `{"target_parent_id":"11","expected_revision":"2"}`
	request("move-event-rollback", backBody, http.StatusInternalServerError, "INTERNAL_ERROR")
	if readFacts() != beforeFailure {
		t.Fatal("事件失败后目录位置、版本、快照或序号未全部回滚")
	}
	if _, err := fixture.pool.ExecContext(ctx, "DROP TRIGGER fixture_move_event_failure"); err != nil {
		t.Fatal(err)
	}
	recovered := request("move-event-rollback", backBody, http.StatusOK, "")
	if recovered.Data.Entry.Revision != "3" ||
		readFacts() != (moveFacts{parentID: 11, revision: 3, childParentID: 14, childRevision: 1, snapshots: 3, events: 2, sequence: 2}) {
		t.Fatal("原键重试未保存预期位置和连续序号")
	}
	verifyEvent(1, first.Data.Entry, "12")
	verifyEvent(2, recovered.Data.Entry, "11")
	logs, err := fixture.stop()
	if err != nil {
		t.Fatal("隔离服务停机失败", err)
	}
	verifyBackupSourceStopped(t, fixture.api.baseURL, logs)
	t.Log("目录 14 的父目录 11→12 写事件 1，后代父项仍为 14。同键、同位置和版本冲突保持日志，回滚后用原键移回 11 写事件 2")
}

// requestServerMoveEntry 验证真实启动后的正式 POST 响应与当前请求 ID。
func requestServerMoveEntry(t *testing.T, baseURL, token, id, key, body string, status int, code, field string) dto.MoveEntryResponse {
	t.Helper()
	// 1. 通过实际 HTTP 发送正式移动请求。
	requestID := uuid.NewString()
	message, err := http.NewRequest("POST", baseURL+"/api/v1/entries/"+id+"/move?note=private-move-query", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	message.Header.Set("Content-Type", "application/json")
	message.Header.Set("Origin", "http://127.0.0.1:5173")
	message.Header.Set("X-Request-ID", strings.ToUpper(requestID))
	if token != "" {
		message.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		message.Header.Set("Idempotency-Key", key)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Do(message)
	if err != nil {
		t.Fatal("正式移动请求失败", err)
	}
	encoded, readError := io.ReadAll(response.Body)
	closeError := response.Body.Close()
	if readError != nil || closeError != nil || response.StatusCode != status || response.Header.Get("Content-Type") != "application/json; charset=utf-8" || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Content-Encoding") != "" || response.Header.Get("X-Request-ID") != requestID || response.Header.Get("Access-Control-Allow-Origin") != "http://127.0.0.1:5173" || response.ContentLength != int64(len(encoded)) || len(response.Cookies()) != 0 {
		t.Fatal("正式移动状态或响应头错误", response.StatusCode, readError, closeError)
	}
	// 2. 校验完整成功快照或固定错误字段。
	var payload dto.MoveEntryResponse
	if status == http.StatusOK {
		if json.Unmarshal(encoded, &payload) != nil || payload.RequestID != requestID || response.Header.Get("Pragma") != "no-cache" {
			t.Fatal("正式移动成功 JSON 错误")
		}
		stable, err := json.Marshal(payload.Data)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := dto.RestoreMoveEntryData(stable); err != nil {
			t.Fatal("正式快照字段错误", err)
		}
		return payload
	}
	var failure dto.ErrorResponse
	if json.Unmarshal(encoded, &failure) != nil || failure.RequestID != requestID || failure.Error.Code != code || failure.Error.Message == "" {
		t.Fatal("正式移动错误响应不符")
	}
	if code == "REVISION_CONFLICT" {
		current, ok := failure.Error.Details["current_revision"].(string)
		value, parseError := strconv.ParseUint(current, 10, 64)
		if !ok || parseError != nil || value == 0 || strconv.FormatUint(value, 10) != current || len(failure.Error.Details) != 1 {
			t.Fatal("正式冲突当前版本错误")
		}
	} else if field != "" {
		if len(failure.Error.Details) != 1 || failure.Error.Details["field"] != field {
			t.Fatal("正式错误字段定位错误")
		}
	} else if failure.Error.Details != nil {
		t.Fatal("普通错误详情非空")
	}
	if (status == http.StatusUnauthorized) != (response.Header.Get("WWW-Authenticate") != "") {
		t.Fatal("认证响应提示与状态不符")
	}
	return payload
}

// verifyServerEntryMove 在启动独占库中验证正式移动、结果保存及后续变更。
func verifyServerEntryMove(t *testing.T, ctx context.Context, pool *sql.DB, baseURL, token, foreignToken string) dto.MoveEntryResponse {
	t.Helper()
	// 1. 准备专用移动夹具，保留各接口已有的验证入口。
	for _, statement := range []string{
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,color_key) VALUES(70002,1,11,'移动源',CAST('移动源' AS BINARY),'DIR',NULL),(70003,1,11,'移动目的地',CAST('移动目的地' AS BINARY),'DIR',NULL),(70004,1,11,'移动后续',CAST('移动后续' AS BINARY),'DIR',NULL),(70005,1,70002,'待移动目录',CAST('待移动目录' AS BINARY),'DIR','green'),(70006,1,70005,'后代目录',CAST('后代目录' AS BINARY),'DIR',NULL),(70007,1,11,'移动冲突目录',CAST('移动冲突目录' AS BINARY),'DIR',NULL),(70008,1,70007,'移动文件.MD',CAST('移动文件.md' AS BINARY),'DIR',NULL)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,extension,mime_type,object_id,file_size,revision,source_device_id,source_device_name,content_modified_at) VALUES(70001,1,70002,'移动文件.md',CAST('移动文件.md' AS BINARY),'FILE','DOCUMENT','md','text/markdown',41,2048,1,13,'正式移动来源',UTC_TIMESTAMP(6)-INTERVAL 1 DAY)",
		"INSERT INTO user_entry_preferences(user_id,entry_id,starred_at,last_opened_at) VALUES(1,70001,UTC_TIMESTAMP(6)-INTERVAL 1 HOUR,UTC_TIMESTAMP(6))",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("准备正式移动夹具失败", err)
		}
	}
	requestServerMoveEntry(t, baseURL, "", "0", "", `{"private-broken":`, 401, "AUTH_REQUIRED", "")
	requestServerMoveEntry(t, baseURL, token, "0", "", `{"private-broken":`, 400, "INVALID_ARGUMENT", "id")
	requestServerMoveEntry(t, baseURL, token, "70001", "", `{"private-broken":`, 400, "INVALID_ARGUMENT", "Idempotency-Key")
	requestServerMoveEntry(t, baseURL, token, "70001", "invalid-move-parent", `{"target_parent_id":"070003","expected_revision":"1"}`, 400, "INVALID_ARGUMENT", "target_parent_id")
	requestServerMoveEntry(t, baseURL, foreignToken, "70001", serverMoveEntryKey, serverMoveEntryBody, 404, "ENTRY_NOT_FOUND", "")
	requestServerMoveEntry(t, baseURL, token, "11", "root-move", serverMoveEntryBody, 403, "ROOT_OPERATION_FORBIDDEN", "")
	requestServerMoveEntry(t, baseURL, token, "70001", "foreign-move-parent", `{"target_parent_id":"33","expected_revision":"1"}`, 404, "ENTRY_NOT_FOUND", "")
	requestServerMoveEntry(t, baseURL, token, "70001", "file-move-parent", `{"target_parent_id":"70001","expected_revision":"1"}`, 400, "INVALID_ARGUMENT", "target_parent_id")
	requestServerMoveEntry(t, baseURL, token, "70005", "self-move-parent", `{"target_parent_id":"70005","expected_revision":"1"}`, 400, "INVALID_ARGUMENT", "target_parent_id")
	requestServerMoveEntry(t, baseURL, token, "70005", "descendant-move-parent", `{"target_parent_id":"70006","expected_revision":"1"}`, 400, "INVALID_ARGUMENT", "target_parent_id")
	requestServerMoveEntry(t, baseURL, token, "70001", "move-name-conflict", `{"target_parent_id":"70007","expected_revision":"1"}`, 409, "NAME_CONFLICT", "")

	// 2. 正式移动和重试核对新父目录、版本及内容与偏好事实。
	first := requestServerMoveEntry(t, baseURL, token, "70001", serverMoveEntryKey, serverMoveEntryBody, 200, "", "")
	entry := first.Data.Entry
	if entry.ID != "70001" || entry.ParentID == nil || *entry.ParentID != "70003" || entry.Name != "移动文件.md" || entry.Revision != "2" || entry.Kind != "document" || entry.Extension == nil || *entry.Extension != "md" || entry.MIMEType == nil || *entry.MIMEType != "text/markdown" || entry.SourceDevice == nil || entry.SourceDevice.Name == nil || *entry.SourceDevice.Name != "正式移动来源" || !entry.Starred || entry.LastOpenedAt == nil {
		t.Fatal("正式文件移动快照错误")
	}
	retry := requestServerMoveEntry(t, baseURL, token, "70001", serverMoveEntryKey, serverMoveEntryBody, 200, "", "")
	if !reflect.DeepEqual(first.Data, retry.Data) || first.RequestID == retry.RequestID {
		t.Fatal("正式移动重试快照或请求 ID 错误")
	}
	requestServerMoveEntry(t, baseURL, token, "70005", serverMoveEntryKey, serverMoveEntryBody, 409, "IDEMPOTENCY_CONFLICT", "")
	requestServerMoveEntry(t, baseURL, token, "70001", serverMoveEntryKey, `{"target_parent_id":"70004","expected_revision":"1"}`, 409, "IDEMPOTENCY_CONFLICT", "")
	requestServerMoveEntry(t, baseURL, token, "70001", "stale-move-version", serverMoveEntryBody, 409, "REVISION_CONFLICT", "")
	folder := requestServerMoveEntry(t, baseURL, token, "70005", "move-folder", serverMoveEntryBody, 200, "", "")
	if folder.Data.Entry.Kind != "folder" || folder.Data.Entry.ParentID == nil || *folder.Data.Entry.ParentID != "70003" || folder.Data.Entry.Revision != "2" || folder.Data.Entry.ColorKey == nil || *folder.Data.Entry.ColorKey != "green" {
		t.Fatal("正式目录移动结果错误")
	}
	noChange := requestServerMoveEntry(t, baseURL, token, "70001", "move-same-position", `{"target_parent_id":"70003","expected_revision":"2"}`, 200, "", "")
	if !reflect.DeepEqual(noChange.Data, first.Data) {
		t.Fatal("正式同位置匹配改变版本或事实")
	}

	// 3. 核对一条幂等记录、资源关联和保留期限。
	var count int
	var resourceID uint64
	var body []byte
	var expiresAt, createdAt time.Time
	if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM idempotency_records WHERE user_id=1 AND operation=? AND idempotency_key=?", domain.MoveEntryOperation, serverMoveEntryKey).Scan(&count); err != nil || count != 1 {
		t.Fatal("正式移动重试产生多份记录", err)
	}
	if err := pool.QueryRowContext(ctx, "SELECT resource_id,response_body,expires_at,created_at FROM idempotency_records WHERE user_id=1 AND operation=? AND idempotency_key=?", domain.MoveEntryOperation, serverMoveEntryKey).Scan(&resourceID, &body, &expiresAt, &createdAt); err != nil {
		t.Fatal(err)
	}
	saved, err := dto.RestoreMoveEntryData(body)
	if err != nil || resourceID != 70001 || !reflect.DeepEqual(saved, &first.Data) || expiresAt.Sub(createdAt) < 7*24*time.Hour {
		t.Fatal("正式移动保存结果、关联或期限错误", err)
	}
	// 4. 后续移动更新当前位置，原始快照供重启后复验。
	later := requestServerMoveEntry(t, baseURL, token, "70001", "move-later", `{"target_parent_id":"70004","expected_revision":"2"}`, 200, "", "")
	if later.Data.Entry.ParentID == nil || *later.Data.Entry.ParentID != "70004" || later.Data.Entry.Revision != "3" || later.Data.Entry.Name != first.Data.Entry.Name {
		t.Fatal("后续正式移动事实错误")
	}
	retry = requestServerMoveEntry(t, baseURL, token, "70001", serverMoveEntryKey, serverMoveEntryBody, 200, "", "")
	if !reflect.DeepEqual(first.Data, retry.Data) {
		t.Fatal("正式移动快照混入后续状态")
	}
	return first
}
