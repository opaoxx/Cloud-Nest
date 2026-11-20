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

const serverPurgeEntryID = "74201"
const serverPurgeEntryPath = "/api/v1/trash/" + serverPurgeEntryID
const serverPurgeEntryKey = "server-purge-directory"
const serverPurgeEntryOrigin = "http://127.0.0.1:5173"

func requestServerPurgeEntry(
	t *testing.T, ctx context.Context, baseURL, token, key string,
	preview dto.PurgePreviewData, status int, code string, details map[string]any,
) dto.PurgeEntryAcceptedResponse {
	t.Helper()
	// 1. 向实际启动的服务提交预检版本、原样凭证和幂等键。
	requestID := uuid.NewString()
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, baseURL+serverPurgeEntryPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Origin", serverPurgeEntryOrigin)
	request.Header.Set("X-Request-ID", strings.ToUpper(requestID))
	request.Header.Set(dto.PurgeExpectedRevisionHeader, preview.Revision)
	request.Header.Set(dto.PurgePreviewDigestHeader, preview.PreviewDigest)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("正式清理确认请求失败", err)
	}
	defer response.Body.Close()
	headers := response.Header
	if response.StatusCode != status || headers.Get("X-Request-ID") != requestID ||
		headers.Get("Cache-Control") != "no-store" || headers.Get("Content-Type") != "application/json; charset=utf-8" ||
		headers.Get("Access-Control-Allow-Origin") != serverPurgeEntryOrigin ||
		(headers.Get("WWW-Authenticate") == `Bearer realm="cloudnest"`) != (status == http.StatusUnauthorized) ||
		(status != http.StatusUnauthorized && headers.Get("Pragma") != "no-cache") {
		t.Fatalf("正式清理确认状态 %d，期望 %d，响应头 %v", response.StatusCode, status, headers)
	}

	// 2. 核对 202 快照或公开错误与本次请求 ID。
	decoder := json.NewDecoder(response.Body)
	if status == http.StatusAccepted {
		var result dto.PurgeEntryAcceptedResponse
		want := dto.PurgeEntryAcceptedData{
			EntryID: serverPurgeEntryID, Status: "PURGING", ProgressURL: serverPurgeEntryPath + "/purge",
		}
		if err := decoder.Decode(&result); err != nil || result.RequestID != requestID || result.Data != want {
			t.Fatal("正式清理确认受理快照不符", result, err)
		}
		return result
	}
	var failure dto.ErrorResponse
	if err := decoder.Decode(&failure); err != nil || failure.RequestID != requestID ||
		failure.Error.Code != code || !reflect.DeepEqual(failure.Error.Details, details) {
		t.Fatal("正式清理确认错误正文不符", failure, err)
	}
	return dto.PurgeEntryAcceptedResponse{}
}

func verifyServerPurgeEntry(t *testing.T, ctx context.Context, pool *sql.DB, baseURL, token, foreignToken string) (dto.PurgePreviewData, dto.PurgeEntryAcceptedResponse) {
	t.Helper()
	// 1. 在独占库准备回收项，并验证该路径的浏览器跨域预检。
	if _, err := pool.ExecContext(ctx, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at)
		VALUES(?,1,11,'清理确认夹具',CAST('清理确认夹具' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6))`, serverPurgeEntryID); err != nil {
		t.Fatal("准备正式清理夹具失败", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodOptions, baseURL+serverPurgeEntryPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Origin", serverPurgeEntryOrigin)
	request.Header.Set("Access-Control-Request-Method", http.MethodDelete)
	request.Header.Set("Access-Control-Request-Headers", "authorization, idempotency-key, x-cloudnest-expected-revision, x-cloudnest-purge-preview")
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("正式路径的跨域预检失败", err)
	}
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	allowedHeaders := response.Header.Get("Access-Control-Allow-Headers")
	if readErr != nil || closeErr != nil || response.StatusCode != http.StatusNoContent || len(body) != 0 ||
		response.Header.Get("Access-Control-Allow-Origin") != serverPurgeEntryOrigin ||
		!strings.Contains(allowedHeaders, dto.PurgeExpectedRevisionHeader) || !strings.Contains(allowedHeaders, dto.PurgePreviewDigestHeader) {
		t.Fatal("正式路径的跨域预检结果不符", response.StatusCode, response.Header, readErr, closeErr)
	}

	// 2. 使用正式预检的凭证验证身份、归属及版本，再确认清理。
	preview := requestServerPurgePreview(t, ctx, baseURL, token, serverPurgeEntryID, 200, "").Data
	if preview.Revision != "1" || preview.DirectoryCount != 1 {
		t.Fatal("正式清理预检未返回夹具的当前版本和范围")
	}
	requestServerPurgeEntry(t, ctx, baseURL, "", "", preview, 401, "AUTH_REQUIRED", nil)
	requestServerPurgeEntry(t, ctx, baseURL, token, "", preview, 400, "INVALID_ARGUMENT", map[string]any{"field": "Idempotency-Key"})
	requestServerPurgeEntry(t, ctx, baseURL, foreignToken, serverPurgeEntryKey, preview, 404, "ENTRY_NOT_FOUND", nil)
	stale := preview
	stale.Revision = "2"
	requestServerPurgeEntry(t, ctx, baseURL, token, serverPurgeEntryKey, stale, 409, "REVISION_CONFLICT", map[string]any{"current_revision": "1"})
	first := requestServerPurgeEntry(t, ctx, baseURL, token, serverPurgeEntryKey, preview, 202, "", nil)
	return preview, first
}

func verifyServerPurgeEntryAfterRestart(
	t *testing.T, ctx context.Context, pool *sql.DB, baseURL, token string,
	preview dto.PurgePreviewData, first dto.PurgeEntryAcceptedResponse,
) {
	t.Helper()
	// 1. 在新实例重试原请求，核对原快照和新请求 ID。
	replay := requestServerPurgeEntry(t, ctx, baseURL, token, serverPurgeEntryKey, preview, 202, "", nil)
	if replay.Data != first.Data || replay.RequestID == first.RequestID {
		t.Fatal("重启后正式清理确认的快照或请求 ID 不符")
	}
	conflict := preview
	conflict.Revision = "2"
	requestServerPurgeEntry(t, ctx, baseURL, token, serverPurgeEntryKey, conflict, 409, "IDEMPOTENCY_CONFLICT", nil)
	requestServerPurgeEntry(t, ctx, baseURL, token, "server-purge-new-key", preview, 404, "ENTRY_NOT_FOUND", nil)

	// 2. 核对受理及完成各递增一次，受理快照仍和公开 data 对应。
	var status string
	var revision uint64
	var startedAt sql.NullTime
	var purgedAt sql.NullTime
	if err := pool.QueryRowContext(ctx, "SELECT status,revision,purge_started_at,purged_at FROM file_entries WHERE user_id=1 AND id=?", serverPurgeEntryID).
		Scan(&status, &revision, &startedAt, &purgedAt); err != nil || status != "PURGED" || revision != 3 || !startedAt.Valid ||
		!purgedAt.Valid || purgedAt.Time.Before(startedAt.Time) {
		t.Fatal("正式清理确认的持久状态不符", status, revision, err)
	}
	var stableBody []byte
	var responseStatus int
	if err := pool.QueryRowContext(ctx, "SELECT response_status,response_body FROM idempotency_records WHERE user_id=1 AND operation=? AND idempotency_key=?",
		domain.PurgeEntryOperation, serverPurgeEntryKey).Scan(&responseStatus, &stableBody); err != nil || responseStatus != 202 {
		t.Fatal("正式清理确认的 202 快照未持久化", err)
	}
	stored, err := dto.DecodePurgeEntryAcceptedData(stableBody)
	if err != nil || *stored != first.Data {
		t.Fatal("正式清理确认交付与已保存快照不符", err)
	}
}
