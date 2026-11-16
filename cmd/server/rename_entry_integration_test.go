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

const serverRenameEntryBody = `{"name":"正式笔记.PDF","expected_revision":"1"}`
const serverRenameEntryKey = "server-rename-007"

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
