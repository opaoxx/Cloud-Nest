package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"reflect"
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
