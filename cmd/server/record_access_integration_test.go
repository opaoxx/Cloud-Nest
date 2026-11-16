package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
)

// requestServerRecordAccess 验证真实启动后的正式 POST 响应与当前请求 ID。
func requestServerRecordAccess(t *testing.T, baseURL, token, id, body string, status int, code, field string) dto.RecordAccessResponse {
	t.Helper()
	// 1. 通过实际 HTTP 发送正式访问请求。
	requestID := uuid.NewString()
	message, err := http.NewRequest("POST", baseURL+"/api/v1/entries/"+id+"/access?note=private-access-query", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	message.Header.Set("Content-Type", "application/json")
	message.Header.Set("Origin", "http://127.0.0.1:5173")
	message.Header.Set("X-Request-ID", strings.ToUpper(requestID))
	if token != "" {
		message.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Do(message)
	if err != nil {
		t.Fatal("正式访问请求失败", err)
	}
	encoded, readError := io.ReadAll(response.Body)
	closeError := response.Body.Close()
	if readError != nil || closeError != nil || response.StatusCode != status || response.Header.Get("Content-Type") != "application/json; charset=utf-8" || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Content-Encoding") != "" || response.Header.Get("X-Request-ID") != requestID || response.Header.Get("Access-Control-Allow-Origin") != "http://127.0.0.1:5173" || response.ContentLength != int64(len(encoded)) || len(response.Cookies()) != 0 {
		t.Fatal("正式访问状态或响应头错误", response.StatusCode, readError, closeError)
	}
	// 2. 核对精确成功字段或统一错误结构。
	var payload dto.RecordAccessResponse
	if status == http.StatusOK {
		var outer map[string]json.RawMessage
		var fields map[string]json.RawMessage
		if json.Unmarshal(encoded, &payload) != nil || payload.RequestID != requestID || response.Header.Get("Pragma") != "no-cache" ||
			json.Unmarshal(encoded, &outer) != nil || len(outer) != 2 || json.Unmarshal(outer["data"], &fields) != nil || len(fields) != 1 || payload.Data.LastOpenedAt.IsZero() {
			t.Fatal("正式访问成功 JSON 错误")
		}
		if payload.Data.LastOpenedAt.Location() != time.UTC || payload.Data.LastOpenedAt.Nanosecond()%1000 != 0 || fields["last_opened_at"] == nil {
			t.Fatal("正式访问时间格式错误")
		}
		return payload
	}
	var failure dto.ErrorResponse
	if json.Unmarshal(encoded, &failure) != nil || failure.RequestID != requestID || failure.Error.Code != code || failure.Error.Message == "" {
		t.Fatal("正式访问错误响应不符")
	}
	if field != "" {
		if len(failure.Error.Details) != 1 || failure.Error.Details["field"] != field {
			t.Fatal("正式访问错误字段不符")
		}
	} else if failure.Error.Details != nil {
		t.Fatal("普通错误详情非空")
	}
	if (status == http.StatusUnauthorized) != (response.Header.Get("WWW-Authenticate") != "") {
		t.Fatal("认证响应提示与状态不符")
	}
	return payload
}

// verifyServerRecordAccess 在启动独占库中验证访问时间、文件范围及原有偏好。
func verifyServerRecordAccess(t *testing.T, ctx context.Context, pool *sql.DB, baseURL, token, foreignToken string) dto.RecordAccessResponse {
	t.Helper()
	// 1. 准备专用文件、目录和已有收藏，保存基础记录。
	for _, statement := range []string{
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(90002,1,11,'访问父目录',CAST('访问父目录' AS BINARY),'DIR')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,extension,mime_type,object_id,file_size,revision,source_device_id,source_device_name) VALUES(90001,1,90002,'访问文件.md',CAST('访问文件.md' AS BINARY),'FILE','DOCUMENT','md','text/markdown',41,2048,9,13,'正式访问来源'),(90003,1,90002,'首次访问.md',CAST('首次访问.md' AS BINARY),'FILE','DOCUMENT','md','text/markdown',41,2048,1,13,'正式访问来源')",
		"INSERT INTO user_entry_preferences(user_id,entry_id,starred_at,created_at,updated_at) VALUES(1,90001,'2020-01-02 03:04:05.123456','2020-01-02 03:04:05.123456','2020-01-02 03:04:05.123456')",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("准备正式访问夹具失败", err)
		}
	}
	store, err := repository.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	baseBefore, err := store.FindUserFileEntry(ctx, 1, 90001)
	if err != nil {
		t.Fatal(err)
	}
	// 2. 核对首次填充、首次插入及 preview/open 窗口合并。
	var before, after time.Time
	if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&before); err != nil {
		t.Fatal(err)
	}
	first := requestServerRecordAccess(t, baseURL, token, "90001", `{"purpose":"preview"}`, 200, "", "")
	repeated := requestServerRecordAccess(t, baseURL, token, "90001", `{"purpose":"open"}`, 200, "", "")
	if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.Data, repeated.Data) || first.RequestID == repeated.RequestID || first.Data.LastOpenedAt.Before(before) || first.Data.LastOpenedAt.After(after) {
		t.Fatal("正式访问首次填充或窗口合并错误")
	}
	inserted := requestServerRecordAccess(t, baseURL, token, "90003", `{"purpose":"open"}`, 200, "", "")
	insertedPreference, err := store.FindUserEntryPreference(ctx, 1, 90003)
	if err != nil || insertedPreference.LastOpenedAt == nil || !inserted.Data.LastOpenedAt.Equal(*insertedPreference.LastOpenedAt) || insertedPreference.StarredAt != nil {
		t.Fatal("正式首次访问偏好插入错误", err)
	}
	// 3. 使用旧访问时间验证窗口外更新。
	if _, err := pool.ExecContext(ctx, "UPDATE user_entry_preferences SET last_opened_at='2020-01-02 03:04:05.123456',updated_at='2020-01-02 03:04:05.123456' WHERE user_id=1 AND entry_id=90001"); err != nil {
		t.Fatal(err)
	}
	final := requestServerRecordAccess(t, baseURL, token, "90001", `{"purpose":"open"}`, 200, "", "")
	if !final.Data.LastOpenedAt.After(first.Data.LastOpenedAt) {
		t.Fatal("窗口外访问未使用当前数据库时间")
	}
	preferenceBeforeErrors, err := store.FindUserEntryPreference(ctx, 1, 90001)
	if err != nil {
		t.Fatal(err)
	}
	var createdBefore, updatedBefore time.Time
	if err := pool.QueryRowContext(ctx, "SELECT created_at,updated_at FROM user_entry_preferences WHERE user_id=1 AND entry_id=90001").Scan(&createdBefore, &updatedBefore); err != nil {
		t.Fatal(err)
	}
	// 4. 核对认证、参数、文件类型和归属错误。
	requestServerRecordAccess(t, baseURL, "", "0", `{"private-broken":`, 401, "AUTH_REQUIRED", "")
	requestServerRecordAccess(t, baseURL, "invalid-access", "90001", `{"purpose":"preview"}`, 401, "AUTH_REQUIRED", "")
	requestServerRecordAccess(t, baseURL, token, "0", `{"private-broken":`, 400, "INVALID_ARGUMENT", "id")
	for _, body := range []string{`{}`, `{"purpose":null}`, `{"purpose":"download"}`} {
		requestServerRecordAccess(t, baseURL, token, "90001", body, 400, "INVALID_ARGUMENT", "purpose")
	}
	for _, id := range []string{"90002", "11"} {
		requestServerRecordAccess(t, baseURL, token, id, `{"purpose":"preview"}`, 400, "INVALID_ARGUMENT", "id")
	}
	requestServerRecordAccess(t, baseURL, foreignToken, "90001", `{"purpose":"preview"}`, 404, "ENTRY_NOT_FOUND", "")
	requestServerRecordAccess(t, baseURL, token, "33", `{"purpose":"open"}`, 404, "ENTRY_NOT_FOUND", "")
	requestServerRecordAccess(t, baseURL, token, "9999999", `{"purpose":"preview"}`, 404, "ENTRY_NOT_FOUND", "")
	// 5. 核对目标及祖先隐藏，并对照数据库偏好和基础事实。
	for _, entryID := range []uint64{90001, 90002} {
		for _, status := range []string{"TRASHED", "PURGING", "PURGED"} {
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status=?,deleted_at=UTC_TIMESTAMP(6),purge_started_at=IF(?='TRASHED',NULL,UTC_TIMESTAMP(6)),purged_at=IF(?='PURGED',UTC_TIMESTAMP(6),NULL),object_id=IF(entry_type='FILE' AND ?='PURGED',NULL,object_id) WHERE id=?", status, status, status, status, entryID); err != nil {
				t.Fatal(err)
			}
			requestServerRecordAccess(t, baseURL, token, "90001", `{"purpose":"preview"}`, 404, "ENTRY_NOT_FOUND", "")
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='ACTIVE',deleted_at=NULL,purge_started_at=NULL,purged_at=NULL,object_id=IF(entry_type='FILE',41,NULL) WHERE id=?", entryID); err != nil {
				t.Fatal(err)
			}
		}
	}
	preferenceAfter, err := store.FindUserEntryPreference(ctx, 1, 90001)
	old := time.Date(2020, 1, 2, 3, 4, 5, 123456000, time.UTC)
	if err != nil || !reflect.DeepEqual(preferenceBeforeErrors, preferenceAfter) || preferenceAfter.StarredAt == nil || !preferenceAfter.StarredAt.Equal(old) || preferenceAfter.LastOpenedAt == nil || !preferenceAfter.LastOpenedAt.Equal(final.Data.LastOpenedAt) {
		t.Fatal("正式访问偏好或错误请求后的数据不符", err)
	}
	var createdAfter, updatedAfter time.Time
	if err := pool.QueryRowContext(ctx, "SELECT created_at,updated_at FROM user_entry_preferences WHERE user_id=1 AND entry_id=90001").Scan(&createdAfter, &updatedAfter); err != nil || !createdBefore.Equal(createdAfter) || !createdAfter.Equal(old) || !updatedBefore.Equal(updatedAfter) || !updatedAfter.Equal(final.Data.LastOpenedAt) {
		t.Fatal("正式访问审计事实不符", err)
	}
	baseAfter, err := store.FindUserFileEntry(ctx, 1, 90001)
	if err != nil || !reflect.DeepEqual(baseBefore, baseAfter) {
		t.Fatal("正式访问后的文件基础事实错误", err)
	}
	var directoryPreferences int
	if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM user_entry_preferences WHERE user_id=1 AND entry_id=90002").Scan(&directoryPreferences); err != nil || directoryPreferences != 0 {
		t.Fatal("目录访问被拒绝后的偏好数量错误", err)
	}
	return final
}
