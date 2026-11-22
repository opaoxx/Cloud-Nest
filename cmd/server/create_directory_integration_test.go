package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

const serverCreateDirectoryBody = `{"parent_id":"15","name":"正式创建 🌱","color_key":"green"}`
const serverCreateDirectoryKey = "server-create-007"

func TestServerDirectoryChangeEventIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_DIRECTORY_CHANGE_EVENT_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_DIRECTORY_CHANGE_EVENT_TEST=1 后启动独占 MySQL、MinIO 和服务")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	fixture := prepareBackupSource(t, ctx)
	t.Logf("目录事件专用 MySQL %s，专用 MinIO %s", fixture.mysqlContainer[:12], fixture.minioContainer[:12])
	userID := backupFixtureID(t, fixture.login.User.ID)

	type directoryFacts struct {
		directories, snapshots, events, sequence uint64
	}
	readFacts := func() directoryFacts {
		t.Helper()
		var facts directoryFacts
		if err := fixture.pool.QueryRowContext(ctx, `SELECT
			(SELECT COUNT(*) FROM file_entries WHERE user_id=users.id AND parent_id IS NOT NULL),
			(SELECT COUNT(*) FROM idempotency_records WHERE user_id=users.id AND operation=?),
			(SELECT COUNT(*) FROM change_events WHERE user_id=users.id), change_seq FROM users WHERE id=?`,
			domain.CreateDirectoryOperation, userID).Scan(&facts.directories, &facts.snapshots, &facts.events, &facts.sequence); err != nil {
			t.Fatal(err)
		}
		return facts
	}
	verifyEvent := func(sequence uint64, entry dto.CreatedDirectorySummary) {
		t.Helper()
		var savedEntryID, revision uint64
		var eventType string
		var payload []byte
		if err := fixture.pool.QueryRowContext(ctx, "SELECT entry_id,revision,event_type,payload FROM change_events WHERE user_id=? AND `sequence`=?",
			userID, sequence).Scan(&savedEntryID, &revision, &eventType, &payload); err != nil {
			t.Fatal(err)
		}
		var fields map[string]string
		want := map[string]string{"entry_id": entry.ID, "parent_id": entry.ParentID, "name": entry.Name, "revision": entry.Revision}
		if err := json.Unmarshal(payload, &fields); err != nil || !reflect.DeepEqual(fields, want) ||
			eventType != "DIRECTORY_CREATED" || strconv.FormatUint(savedEntryID, 10) != entry.ID ||
			strconv.FormatUint(revision, 10) != entry.Revision {
			t.Fatal("目录事件表列或载荷与已创建目录不符", string(payload), err)
		}
	}
	const path = "/api/v1/directories"

	// 1. 通过正式入口创建，再用相同幂等键重放，核对只有一条变更事件。
	if facts := readFacts(); facts != (directoryFacts{}) {
		t.Fatal("合成账号应从空目录事件和幂等记录开始", facts)
	}
	body := fmt.Sprintf(`{"parent_id":%q,"name":"  同步 Cafe\u0301 🌱  ","color_key":"green"}`, fixture.login.RootEntryID)
	var first, replay dto.CreateDirectoryResponse
	fixture.api.request(t, ctx, http.MethodPost, path, "directory-event-first", strings.NewReader(body), http.StatusCreated, &first)
	if first.RequestID == "" || first.Data.Entry.ParentID != fixture.login.RootEntryID ||
		first.Data.Entry.Name != "同步 Cafe\u0301 🌱" || first.Data.Entry.Revision != "1" {
		t.Fatal("新建目录响应与预期不符", first.Data)
	}
	verifyEvent(1, first.Data.Entry)
	fixture.api.request(t, ctx, http.MethodPost, path, "directory-event-first", strings.NewReader(body), http.StatusCreated, &replay)
	if !reflect.DeepEqual(first.Data, replay.Data) || replay.RequestID == "" || first.RequestID == replay.RequestID ||
		readFacts() != (directoryFacts{directories: 1, snapshots: 1, events: 1, sequence: 1}) {
		t.Fatal("重复请求改写了快照或产生了第二次业务写入")
	}

	// 2. 仅在新目录、幂等记录和序号都已经写入的事务中注入事件失败。
	if _, err := fixture.pool.ExecContext(ctx, `CREATE TRIGGER fixture_directory_event_failure
		BEFORE INSERT ON change_events FOR EACH ROW
		BEGIN
			IF EXISTS (SELECT 1 FROM file_entries WHERE user_id=NEW.user_id AND id=NEW.entry_id AND name='同步回滚目录')
				AND EXISTS (SELECT 1 FROM idempotency_records WHERE user_id=NEW.user_id AND resource_id=NEW.entry_id
					AND idempotency_key='directory-event-rollback')
				AND EXISTS (SELECT 1 FROM users WHERE id=NEW.user_id AND change_seq=2) THEN
				SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='fixture directory event insert failure';
			END IF;
		END`); err != nil {
		t.Fatal("无法安装目录事件隔离故障", err)
	}
	rollbackBody := fmt.Sprintf(`{"parent_id":%q,"name":"同步回滚目录"}`, fixture.login.RootEntryID)
	var failure dto.ErrorResponse
	fixture.api.request(t, ctx, http.MethodPost, path, "directory-event-rollback", strings.NewReader(rollbackBody), http.StatusInternalServerError, &failure)
	if failure.Error.Code != "INTERNAL_ERROR" || readFacts() != (directoryFacts{directories: 1, snapshots: 1, events: 1, sequence: 1}) {
		t.Fatal("追加失败未回滚目录、幂等快照、事件和序号", failure.Error.Code)
	}

	// 3. 解除隔离故障，用原请求重试，核对连续序号和最终事件。
	if _, err := fixture.pool.ExecContext(ctx, "DROP TRIGGER fixture_directory_event_failure"); err != nil {
		t.Fatal(err)
	}
	var recovered dto.CreateDirectoryResponse
	fixture.api.request(t, ctx, http.MethodPost, path, "directory-event-rollback", strings.NewReader(rollbackBody), http.StatusCreated, &recovered)
	if recovered.Data.Entry.Name != "同步回滚目录" ||
		readFacts() != (directoryFacts{directories: 2, snapshots: 2, events: 2, sequence: 2}) {
		t.Fatal("故障解除后重试未形成预期目录、快照和事件")
	}
	verifyEvent(2, recovered.Data.Entry)
	logs, err := fixture.stop()
	if err != nil {
		t.Fatal("隔离服务停机失败", err)
	}
	verifyBackupSourceStopped(t, fixture.api.baseURL, logs)
	t.Logf("首次目录 %s 写 DIRECTORY_CREATED 序号 1，同键重放仍为一条；写入故障回滚，原键重试目录 %s 使用序号 2",
		first.Data.Entry.ID, recovered.Data.Entry.ID)
}

// requestServerCreateDirectory 验证生产启动后的正式创建接口及统一响应。
func requestServerCreateDirectory(t *testing.T, baseURL, token, key, body string, status int, code, field string) dto.CreateDirectoryResponse {
	t.Helper()
	// 1. 向已启动服务发送带当前请求 ID 的实际 POST。
	requestID := uuid.NewString()
	message, err := http.NewRequest("POST", baseURL+"/api/v1/directories?note=private-create-query", strings.NewReader(body))
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
		t.Fatal("正式创建请求失败", err)
	}
	encoded, readError := io.ReadAll(response.Body)
	closeError := response.Body.Close()
	if readError != nil || closeError != nil || response.StatusCode != status || response.Header.Get("Content-Type") != "application/json; charset=utf-8" ||
		response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Content-Encoding") != "" || response.Header.Get("X-Request-ID") != requestID ||
		response.Header.Get("Access-Control-Allow-Origin") != "http://127.0.0.1:5173" || len(response.Cookies()) != 0 || response.ContentLength != int64(len(encoded)) {
		t.Fatal("正式创建状态、传输或响应头错误", response.StatusCode, readError, closeError)
	}

	// 2. 核对完整创建响应或固定业务错误及当前请求 ID。
	var payload dto.CreateDirectoryResponse
	if status == http.StatusCreated {
		if json.Unmarshal(encoded, &payload) != nil || payload.RequestID != requestID || response.Header.Get("Pragma") != "no-cache" {
			t.Fatal("正式创建成功 JSON 错误")
		}
		stable, err := json.Marshal(payload.Data)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := dto.RestoreCreateDirectoryData(stable); err != nil {
			t.Fatal("正式创建快照字段错误", err)
		}
		return payload
	}
	var failure dto.ErrorResponse
	if json.Unmarshal(encoded, &failure) != nil || failure.Error.Code != code || failure.RequestID != requestID || failure.Error.Message == "" {
		t.Fatal("正式创建错误响应不符")
	}
	if field == "" {
		if failure.Error.Details != nil {
			t.Fatal("普通创建错误 details 非空")
		}
	} else if len(failure.Error.Details) != 1 || failure.Error.Details["field"] != field {
		t.Fatal("创建错误字段定位错误")
	}
	if (status == http.StatusUnauthorized) != (response.Header.Get("WWW-Authenticate") != "") {
		t.Fatal("创建认证提示与状态不符")
	}
	return payload
}

// verifyServerDirectoryCreation 在既有启动独占库中核对创建、复用与数据库事实。
func verifyServerDirectoryCreation(t *testing.T, ctx context.Context, pool *sql.DB, baseURL, token, foreignToken string) dto.CreateDirectoryResponse {
	t.Helper()
	// 1. 核对正式认证、幂等头、父目录范围及名称规则。
	requestServerCreateDirectory(t, baseURL, "", "", `{"private-broken":`, 401, "AUTH_REQUIRED", "")
	requestServerCreateDirectory(t, baseURL, token, "", `{"private-broken":`, 400, "INVALID_ARGUMENT", "Idempotency-Key")
	requestServerCreateDirectory(t, baseURL, token, "invalid-name", `{"parent_id":"15","name":"CON.txt"}`, 400, "INVALID_NAME", "name")
	requestServerCreateDirectory(t, baseURL, token, "foreign-parent", `{"parent_id":"33","name":"正式范围校验"}`, 404, "ENTRY_NOT_FOUND", "")
	requestServerCreateDirectory(t, baseURL, token, "file-parent", `{"parent_id":"42","name":"正式文件父项"}`, 400, "INVALID_ARGUMENT", "parent_id")
	first := requestServerCreateDirectory(t, baseURL, token, serverCreateDirectoryKey, serverCreateDirectoryBody, 201, "", "")
	retry := requestServerCreateDirectory(t, baseURL, token, serverCreateDirectoryKey, `{"name":"  正式创建 🌱  ","color_key":"green","parent_id":"15"}`, 201, "", "")
	if !reflect.DeepEqual(first.Data, retry.Data) || first.RequestID == retry.RequestID {
		t.Fatal("正式创建与同键重试的快照或请求 ID 错误")
	}
	requestServerCreateDirectory(t, baseURL, token, serverCreateDirectoryKey, `{"parent_id":"15","name":"正式不同参数"}`, 409, "IDEMPOTENCY_CONFLICT", "")
	requestServerCreateDirectory(t, baseURL, token, "another-create-key", serverCreateDirectoryBody, 409, "NAME_CONFLICT", "")
	requestServerCreateDirectory(t, baseURL, foreignToken, serverCreateDirectoryKey, serverCreateDirectoryBody, 404, "ENTRY_NOT_FOUND", "")

	// 2. 对照正式表核对单条结果、资源 ID、JSON 及保留期限。
	var recordCount int
	if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM idempotency_records WHERE user_id=1 AND operation=? AND idempotency_key=?", domain.CreateDirectoryOperation, serverCreateDirectoryKey).Scan(&recordCount); err != nil || recordCount != 1 {
		t.Fatal("正式创建重试未保留单条结果", err)
	}
	var resourceID uint64
	var body []byte
	var expiresAt, createdAt time.Time
	if err := pool.QueryRowContext(ctx, "SELECT resource_id,response_body,expires_at,created_at FROM idempotency_records WHERE user_id=1 AND operation=? AND idempotency_key=?", domain.CreateDirectoryOperation, serverCreateDirectoryKey).Scan(&resourceID, &body, &expiresAt, &createdAt); err != nil {
		t.Fatal(err)
	}
	saved, err := dto.RestoreCreateDirectoryData(body)
	if err != nil || !reflect.DeepEqual(saved, &first.Data) || strconv.FormatUint(resourceID, 10) != first.Data.Entry.ID || expiresAt.Sub(createdAt) < 7*24*time.Hour {
		t.Fatal("生产装配保存的快照、资源或期限错误", err)
	}

	// 3. 变更当前目录后继续复用原始创建快照，供重启后的再次复验使用。
	nameKey, err := service.BuildEntryNameKey("正式创建后改名")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET name='正式创建后改名',name_key=?,color_key='sand',revision=revision+1 WHERE id=? AND user_id=1", nameKey, resourceID); err != nil {
		t.Fatal(err)
	}
	retry = requestServerCreateDirectory(t, baseURL, token, serverCreateDirectoryKey, serverCreateDirectoryBody, 201, "", "")
	if !reflect.DeepEqual(first.Data, retry.Data) {
		t.Fatal("正式重试混入当前目录变化")
	}
	return first
}
