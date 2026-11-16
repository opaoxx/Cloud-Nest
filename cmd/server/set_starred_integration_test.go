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

// requestServerSetStarred 验证真实启动后的正式 PUT 响应与当前请求 ID。
func requestServerSetStarred(t *testing.T, baseURL, token, id, body string, status int, code, field string) dto.SetStarredResponse {
	t.Helper()
	// 1. 通过实际 HTTP 发送正式收藏请求。
	requestID := uuid.NewString()
	message, err := http.NewRequest("PUT", baseURL+"/api/v1/entries/"+id+"/star?note=private-star-query", strings.NewReader(body))
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
		t.Fatal("正式收藏请求失败", err)
	}
	encoded, readError := io.ReadAll(response.Body)
	closeError := response.Body.Close()
	if readError != nil || closeError != nil || response.StatusCode != status || response.Header.Get("Content-Type") != "application/json; charset=utf-8" || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Content-Encoding") != "" || response.Header.Get("X-Request-ID") != requestID || response.Header.Get("Access-Control-Allow-Origin") != "http://127.0.0.1:5173" || response.ContentLength != int64(len(encoded)) || len(response.Cookies()) != 0 {
		t.Fatal("正式收藏状态或响应头错误", response.StatusCode, readError, closeError)
	}
	// 2. 核对精确成功字段或统一错误结构。
	var payload dto.SetStarredResponse
	if status == http.StatusOK {
		var outer map[string]json.RawMessage
		var fields map[string]json.RawMessage
		if json.Unmarshal(encoded, &payload) != nil || payload.RequestID != requestID || response.Header.Get("Pragma") != "no-cache" ||
			json.Unmarshal(encoded, &outer) != nil || len(outer) != 2 || json.Unmarshal(outer["data"], &fields) != nil || len(fields) != 2 || (payload.Data.StarredAt != nil) != payload.Data.Starred {
			t.Fatal("正式收藏成功 JSON 错误")
		}
		if payload.Data.StarredAt != nil && (payload.Data.StarredAt.Location() != time.UTC || payload.Data.StarredAt.Nanosecond()%1000 != 0) {
			t.Fatal("正式收藏时间格式错误")
		}
		if !payload.Data.Starred && string(fields["starred_at"]) != "null" {
			t.Fatal("取消收藏缺少显式 null")
		}
		return payload
	}
	var failure dto.ErrorResponse
	if json.Unmarshal(encoded, &failure) != nil || failure.RequestID != requestID || failure.Error.Code != code || failure.Error.Message == "" {
		t.Fatal("正式收藏错误响应不符")
	}
	if field != "" {
		if len(failure.Error.Details) != 1 || failure.Error.Details["field"] != field {
			t.Fatal("正式收藏错误字段不符")
		}
	} else if failure.Error.Details != nil {
		t.Fatal("普通错误详情非空")
	}
	if (status == http.StatusUnauthorized) != (response.Header.Get("WWW-Authenticate") != "") {
		t.Fatal("认证响应提示与状态不符")
	}
	return payload
}

// verifyServerSetStarred 在启动独占库中验证收藏、取消、范围和偏好事实。
func verifyServerSetStarred(t *testing.T, ctx context.Context, pool *sql.DB, baseURL, token, foreignToken string) dto.SetStarredResponse {
	t.Helper()
	// 1. 准备专用文件和目录，保存目录项基础及最近访问事实。
	for _, statement := range []string{
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(80002,1,11,'收藏父目录',CAST('收藏父目录' AS BINARY),'DIR'),(80003,1,80002,'收藏目录',CAST('收藏目录' AS BINARY),'DIR')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,extension,mime_type,object_id,file_size,revision,source_device_id,source_device_name) VALUES(80001,1,80002,'收藏文件.md',CAST('收藏文件.md' AS BINARY),'FILE','DOCUMENT','md','text/markdown',41,2048,7,13,'正式收藏来源')",
		"INSERT INTO user_entry_preferences(user_id,entry_id,last_opened_at) VALUES(1,80001,'2020-01-02 03:04:05.123456')",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("准备正式收藏夹具失败", err)
		}
	}
	store, err := repository.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	baseBefore, err := store.FindUserFileEntry(ctx, 1, 80001)
	if err != nil {
		t.Fatal(err)
	}
	// 2. 正式认证、路径和布尔字段校验，以及当前用户范围。
	requestServerSetStarred(t, baseURL, "", "0", `{"private-broken":`, 401, "AUTH_REQUIRED", "")
	requestServerSetStarred(t, baseURL, token, "0", `{"private-broken":`, 400, "INVALID_ARGUMENT", "id")
	requestServerSetStarred(t, baseURL, token, "80001", `{}`, 400, "INVALID_ARGUMENT", "starred")
	requestServerSetStarred(t, baseURL, token, "80001", `{"starred":null}`, 400, "INVALID_ARGUMENT", "starred")
	requestServerSetStarred(t, baseURL, foreignToken, "80001", `{"starred":true}`, 404, "ENTRY_NOT_FOUND", "")
	requestServerSetStarred(t, baseURL, token, "33", `{"starred":true}`, 404, "ENTRY_NOT_FOUND", "")
	requestServerSetStarred(t, baseURL, token, "9999999", `{"starred":true}`, 404, "ENTRY_NOT_FOUND", "")
	// 3. 首次、重复、取消及缺失偏好按明确状态返回。
	first := requestServerSetStarred(t, baseURL, token, "80001", `{"starred":true}`, 200, "", "")
	repeated := requestServerSetStarred(t, baseURL, token, "80001", `{"starred":true}`, 200, "", "")
	if !first.Data.Starred || first.Data.StarredAt == nil || !reflect.DeepEqual(first.Data, repeated.Data) || first.RequestID == repeated.RequestID {
		t.Fatal("正式首次或重复收藏事实错误")
	}
	for range 2 {
		result := requestServerSetStarred(t, baseURL, token, "80001", `{"starred":false}`, 200, "", "")
		if result.Data.Starred || result.Data.StarredAt != nil {
			t.Fatal("正式取消收藏错误")
		}
	}
	missing := requestServerSetStarred(t, baseURL, token, "80003", `{"starred":false}`, 200, "", "")
	var count int
	if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM user_entry_preferences WHERE user_id=1 AND entry_id=80003").Scan(&count); err != nil || count != 0 || missing.Data.Starred || missing.Data.StarredAt != nil {
		t.Fatal("缺失偏好取消收藏错误", err)
	}
	for _, id := range []string{"80003", "11"} {
		result := requestServerSetStarred(t, baseURL, token, id, `{"starred":true}`, 200, "", "")
		if !result.Data.Starred || result.Data.StarredAt == nil {
			t.Fatal("正式目录或根目录收藏错误")
		}
	}
	requestServerSetStarred(t, baseURL, token, "11", `{"starred":false}`, 200, "", "")
	// 4. 目标及祖先的三种隐藏状态统一拒绝。
	for _, entryID := range []uint64{80001, 80002} {
		for _, status := range []string{"TRASHED", "PURGING", "PURGED"} {
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status=?,deleted_at=UTC_TIMESTAMP(6),purge_started_at=IF(?='TRASHED',NULL,UTC_TIMESTAMP(6)),purged_at=IF(?='PURGED',UTC_TIMESTAMP(6),NULL),object_id=IF(entry_type='FILE' AND ?='PURGED',NULL,object_id) WHERE id=?", status, status, status, status, entryID); err != nil {
				t.Fatal(err)
			}
			requestServerSetStarred(t, baseURL, token, "80001", `{"starred":true}`, 404, "ENTRY_NOT_FOUND", "")
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='ACTIVE',deleted_at=NULL,purge_started_at=NULL,purged_at=NULL,object_id=IF(entry_type='FILE',41,NULL) WHERE id=?", entryID); err != nil {
				t.Fatal(err)
			}
		}
	}
	// 5. 最终收藏供重启复验，并独立核对数据库偏好及基础记录。
	final := requestServerSetStarred(t, baseURL, token, "80001", `{"starred":true}`, 200, "", "")
	preference, err := store.FindUserEntryPreference(ctx, 1, 80001)
	if err != nil || preference.StarredAt == nil || !final.Data.Starred || final.Data.StarredAt == nil || !final.Data.StarredAt.Equal(*preference.StarredAt) || preference.LastOpenedAt == nil || !preference.LastOpenedAt.Equal(time.Date(2020, 1, 2, 3, 4, 5, 123456000, time.UTC)) {
		t.Fatal("正式收藏偏好与数据库事实错误", err)
	}
	baseAfter, err := store.FindUserFileEntry(ctx, 1, 80001)
	if err != nil || !reflect.DeepEqual(baseBefore, baseAfter) {
		t.Fatal("收藏与目录项基础事实混用", err)
	}
	return final
}
