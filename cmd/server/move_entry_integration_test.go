package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
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
