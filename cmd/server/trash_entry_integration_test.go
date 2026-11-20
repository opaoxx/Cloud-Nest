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
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

const serverTrashDirectoryID = "74002"
const serverTrashDirectoryKey = "server-trash-directory"
const serverTrashDirectoryBody = `{"expected_revision":"1"}`

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
