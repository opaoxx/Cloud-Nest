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

const serverRenameEntryBody = `{"name":"正式笔记.PDF","expected_revision":"1"}`
const serverRenameEntryKey = "server-rename-007"

func TestServerRenameChangeEventIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_RENAME_CHANGE_EVENT_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_RENAME_CHANGE_EVENT_TEST=1 后启动独占 MySQL、MinIO 和服务")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	fixture := prepareBackupSource(t, ctx)
	t.Logf("重命名事件专用 MySQL %s，专用 MinIO %s", fixture.mysqlContainer[:12], fixture.minioContainer[:12])
	userID := backupFixtureID(t, fixture.login.User.ID)
	parentID := backupFixtureID(t, fixture.login.RootEntryID)
	result, err := fixture.pool.ExecContext(ctx, `INSERT INTO file_entries(user_id,parent_id,name,name_key,entry_type)
		VALUES(?,?,'同步原名',CAST('同步原名' AS BINARY),'DIR')`, userID, parentID)
	if err != nil {
		t.Fatal("无法准备尚无历史事件的合成目录", err)
	}
	entryID, err := result.LastInsertId()
	if err != nil || entryID <= 0 {
		t.Fatal("未取得合成目录 ID", err)
	}
	entryIDText := strconv.FormatInt(entryID, 10)
	request := func(key, body string, status int, code string) dto.RenameEntryResponse {
		t.Helper()
		return requestServerRenameEntry(t, fixture.api.baseURL, fixture.api.accessToken, entryIDText, key, body, status, code, "")
	}
	type renameFacts struct {
		name                                  string
		revision, snapshots, events, sequence uint64
	}
	readFacts := func() renameFacts {
		t.Helper()
		var facts renameFacts
		if err := fixture.pool.QueryRowContext(ctx, `SELECT e.name,e.revision,
			(SELECT COUNT(*) FROM idempotency_records WHERE user_id=u.id AND operation=?),
			(SELECT COUNT(*) FROM change_events WHERE user_id=u.id),u.change_seq
			FROM users u JOIN file_entries e ON e.user_id=u.id WHERE u.id=? AND e.id=?`,
			domain.RenameEntryOperation, userID, entryID).Scan(&facts.name, &facts.revision, &facts.snapshots, &facts.events, &facts.sequence); err != nil {
			t.Fatal(err)
		}
		return facts
	}
	verifyEvent := func(sequence uint64, entry dto.EntryDetailData) {
		t.Helper()
		var savedID, revision uint64
		var eventType string
		var payload []byte
		if err := fixture.pool.QueryRowContext(ctx, "SELECT entry_id,revision,event_type,payload FROM change_events WHERE user_id=? AND `sequence`=?",
			userID, sequence).Scan(&savedID, &revision, &eventType, &payload); err != nil {
			t.Fatal(err)
		}
		var fields map[string]string
		want := map[string]string{"entry_id": entryIDText, "parent_id": fixture.login.RootEntryID, "name": entry.Name, "revision": entry.Revision}
		if err := json.Unmarshal(payload, &fields); err != nil || !reflect.DeepEqual(fields, want) ||
			eventType != "ENTRY_RENAMED" || savedID != uint64(entryID) || strconv.FormatUint(revision, 10) != entry.Revision {
			t.Fatal("重命名事件表列或载荷和事务结果不一致", string(payload), err)
		}
	}

	// 1. 首次改名写入事件，同键重放、版本冲突和同名请求保持原日志。
	if readFacts() != (renameFacts{name: "同步原名", revision: 1}) {
		t.Fatal("合成目录初始状态不符")
	}
	const firstBody = `{"name":"  同步新名 🌱  ","expected_revision":"1"}`
	first := request("rename-event-first", firstBody, http.StatusOK, "")
	if first.Data.Entry.ID != entryIDText || first.Data.Entry.Name != "同步新名 🌱" || first.Data.Entry.Revision != "2" ||
		first.Data.Entry.ParentID == nil || *first.Data.Entry.ParentID != fixture.login.RootEntryID {
		t.Fatal("首次改名的目录身份、名称、父目录或版本不符", first.Data)
	}
	verifyEvent(1, first.Data.Entry)
	replay := request("rename-event-first", `{"name":"同步新名 🌱","expected_revision":"1"}`, http.StatusOK, "")
	request("rename-event-stale", `{"name":"旧版本不改名","expected_revision":"1"}`, http.StatusConflict, "REVISION_CONFLICT")
	if !reflect.DeepEqual(replay.Data, first.Data) || replay.RequestID == first.RequestID ||
		readFacts() != (renameFacts{name: "同步新名 🌱", revision: 2, snapshots: 1, events: 1, sequence: 1}) {
		t.Fatal("重放或版本冲突改变了已提交事实")
	}
	unchanged := request("rename-event-unchanged", `{"name":"同步新名 🌱","expected_revision":"2"}`, http.StatusOK, "")
	beforeFailure := renameFacts{name: "同步新名 🌱", revision: 2, snapshots: 2, events: 1, sequence: 1}
	if !reflect.DeepEqual(unchanged.Data, first.Data) || readFacts() != beforeFailure {
		t.Fatal("同名请求改变了目录版本或追加了事件")
	}

	// 2. 在改名、幂等快照和序号都写入后，触发事件故障，验证整体回滚。
	if _, err := fixture.pool.ExecContext(ctx, `CREATE TRIGGER fixture_rename_event_failure
		BEFORE INSERT ON change_events FOR EACH ROW
		BEGIN
			IF NEW.event_type='ENTRY_RENAMED'
				AND EXISTS (SELECT 1 FROM file_entries WHERE user_id=NEW.user_id AND id=NEW.entry_id
					AND name='同步回滚名' AND revision=3)
				AND EXISTS (SELECT 1 FROM idempotency_records WHERE user_id=NEW.user_id AND resource_id=NEW.entry_id
					AND idempotency_key='rename-event-rollback')
				AND EXISTS (SELECT 1 FROM users WHERE id=NEW.user_id AND change_seq=2) THEN
				SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='fixture rename event insert failure';
			END IF;
		END`); err != nil {
		t.Fatal("无法安装重命名事件隔离故障", err)
	}
	const faultBody = `{"name":"同步回滚名","expected_revision":"2"}`
	request("rename-event-rollback", faultBody, http.StatusInternalServerError, "INTERNAL_ERROR")
	if readFacts() != beforeFailure {
		t.Fatal("事件失败后目录、幂等快照或序号未整体回滚")
	}
	if _, err := fixture.pool.ExecContext(ctx, "DROP TRIGGER fixture_rename_event_failure"); err != nil {
		t.Fatal(err)
	}

	// 3. 解除故障后用原键重试，确认序号连续，历史事件仍保留第一次结果。
	recovered := request("rename-event-rollback", faultBody, http.StatusOK, "")
	if recovered.Data.Entry.ID != entryIDText || recovered.Data.Entry.Revision != "3" ||
		readFacts() != (renameFacts{name: "同步回滚名", revision: 3, snapshots: 3, events: 2, sequence: 2}) {
		t.Fatal("故障恢复未保存预期的新名称、版本和事件")
	}
	verifyEvent(1, first.Data.Entry)
	verifyEvent(2, recovered.Data.Entry)
	logs, err := fixture.stop()
	if err != nil {
		t.Fatal("隔离服务停机失败", err)
	}
	verifyBackupSourceStopped(t, fixture.api.baseURL, logs)
	t.Logf("目录项 %s 改名版本 1→2 写事件 1；同键、版本冲突及同名请求均保持日志；故障回滚后重试以版本 3 写事件 2", entryIDText)
}

// requestServerRenameEntry 验证真实启动后的正式 PATCH 响应与当前请求 ID。
func requestServerRenameEntry(t *testing.T, baseURL, token, id, key, body string, status int, code, field string) dto.RenameEntryResponse {
	t.Helper()
	// 1. 通过实际 HTTP 发送正式重命名请求。
	requestID := uuid.NewString()
	message, err := http.NewRequest("PATCH", baseURL+"/api/v1/entries/"+id+"?note=private-rename-query", strings.NewReader(body))
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
		t.Fatal("正式重命名请求失败", err)
	}
	encoded, readError := io.ReadAll(response.Body)
	closeError := response.Body.Close()
	if readError != nil || closeError != nil || response.StatusCode != status || response.Header.Get("Content-Type") != "application/json; charset=utf-8" || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Content-Encoding") != "" || response.Header.Get("X-Request-ID") != requestID || response.Header.Get("Access-Control-Allow-Origin") != "http://127.0.0.1:5173" || response.ContentLength != int64(len(encoded)) || len(response.Cookies()) != 0 {
		t.Fatal("正式重命名状态或响应头错误", response.StatusCode, readError, closeError)
	}
	// 2. 校验完整成功快照或固定错误字段。
	var payload dto.RenameEntryResponse
	if status == http.StatusOK {
		if json.Unmarshal(encoded, &payload) != nil || payload.RequestID != requestID || response.Header.Get("Pragma") != "no-cache" {
			t.Fatal("正式重命名成功 JSON 错误")
		}
		stable, err := json.Marshal(payload.Data)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := dto.RestoreRenameEntryData(stable); err != nil {
			t.Fatal("正式快照字段错误", err)
		}
		return payload
	}
	var failure dto.ErrorResponse
	if json.Unmarshal(encoded, &failure) != nil || failure.RequestID != requestID || failure.Error.Code != code || failure.Error.Message == "" {
		t.Fatal("正式重命名错误响应不符")
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

// verifyServerEntryRename 在启动独占库中验证正式路由、快照保存及后续变更。
func verifyServerEntryRename(t *testing.T, ctx context.Context, pool *sql.DB, baseURL, token, foreignToken string) dto.RenameEntryResponse {
	t.Helper()
	// 1. 正式认证、输入顺序、范围和根目录规则。
	requestServerRenameEntry(t, baseURL, "", "0", "", `{"private-broken":`, 401, "AUTH_REQUIRED", "")
	requestServerRenameEntry(t, baseURL, token, "0", "", `{"private-broken":`, 400, "INVALID_ARGUMENT", "id")
	requestServerRenameEntry(t, baseURL, token, "42", "", `{"private-broken":`, 400, "INVALID_ARGUMENT", "Idempotency-Key")
	requestServerRenameEntry(t, baseURL, token, "42", "invalid-rename-name", `{"name":"CON.txt","expected_revision":"1"}`, 400, "INVALID_NAME", "name")
	requestServerRenameEntry(t, baseURL, foreignToken, "42", serverRenameEntryKey, serverRenameEntryBody, 404, "ENTRY_NOT_FOUND", "")
	requestServerRenameEntry(t, baseURL, token, "11", "root-rename", `{"name":"新根目录","expected_revision":"1"}`, 403, "ROOT_OPERATION_FORBIDDEN", "")
	first := requestServerRenameEntry(t, baseURL, token, "42", serverRenameEntryKey, serverRenameEntryBody, 200, "", "")
	if first.Data.Entry.ID != "42" || first.Data.Entry.Name != "正式笔记.PDF" || first.Data.Entry.Revision != "2" || first.Data.Entry.Kind != "document" || first.Data.Entry.Extension == nil || *first.Data.Entry.Extension != "pdf" {
		t.Fatal("正式文件改名快照错误")
	}
	retry := requestServerRenameEntry(t, baseURL, token, "42", serverRenameEntryKey, `{"name":"  正式笔记.PDF  ","expected_revision":"1"}`, 200, "", "")
	if !reflect.DeepEqual(first.Data, retry.Data) || first.RequestID == retry.RequestID {
		t.Fatal("正式重试未复用当次快照")
	}
	requestServerRenameEntry(t, baseURL, token, "15", serverRenameEntryKey, serverRenameEntryBody, 409, "IDEMPOTENCY_CONFLICT", "")
	requestServerRenameEntry(t, baseURL, token, "42", "stale-rename-version", serverRenameEntryBody, 409, "REVISION_CONFLICT", "")
	requestServerRenameEntry(t, baseURL, token, "15", "rename-name-conflict", serverRenameEntryBody, 409, "NAME_CONFLICT", "")
	folder := requestServerRenameEntry(t, baseURL, token, "15", "rename-folder", `{"name":"正式目录.PDF","expected_revision":"1"}`, 200, "", "")
	if folder.Data.Entry.Kind != "folder" || folder.Data.Entry.Revision != "2" || folder.Data.Entry.Extension != nil {
		t.Fatal("正式目录改名结果错误")
	}
	noChange := requestServerRenameEntry(t, baseURL, token, "42", "rename-same-name", `{"name":"正式笔记.PDF","expected_revision":"2"}`, 200, "", "")
	if !reflect.DeepEqual(noChange.Data, first.Data) {
		t.Fatal("正式同名匹配改变版本或事实")
	}

	// 2. 幂等快照与实际资源关联并至少保留七天。
	var count int
	var resourceID uint64
	var body []byte
	var expiresAt, createdAt time.Time
	if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM idempotency_records WHERE user_id=1 AND operation=? AND idempotency_key=?", domain.RenameEntryOperation, serverRenameEntryKey).Scan(&count); err != nil || count != 1 {
		t.Fatal("正式重试产生多份记录", err)
	}
	if err := pool.QueryRowContext(ctx, "SELECT resource_id,response_body,expires_at,created_at FROM idempotency_records WHERE user_id=1 AND operation=? AND idempotency_key=?", domain.RenameEntryOperation, serverRenameEntryKey).Scan(&resourceID, &body, &expiresAt, &createdAt); err != nil {
		t.Fatal(err)
	}
	saved, err := dto.RestoreRenameEntryData(body)
	if err != nil || resourceID != 42 || !reflect.DeepEqual(saved, &first.Data) || expiresAt.Sub(createdAt) < 7*24*time.Hour {
		t.Fatal("正式保存结果、关联或期限错误", err)
	}
	// 3. 后续正式改名更新当前状态，原始快照仍可用于重启后复验。
	later := requestServerRenameEntry(t, baseURL, token, "42", "rename-later", `{"name":"正式后续.ZIP","expected_revision":"2"}`, 200, "", "")
	if later.Data.Entry.Revision != "3" || later.Data.Entry.Kind != "archive" || later.Data.Entry.Extension == nil || *later.Data.Entry.Extension != "zip" {
		t.Fatal("后续正式改名派生字段错误")
	}
	retry = requestServerRenameEntry(t, baseURL, token, "42", serverRenameEntryKey, serverRenameEntryBody, 200, "", "")
	if !reflect.DeepEqual(first.Data, retry.Data) {
		t.Fatal("正式快照混入后续改名")
	}
	return first
}
