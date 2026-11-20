package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

func requestServerPurgeEntryProgress(
	t *testing.T, ctx context.Context, baseURL, path, token string, status int, code string,
) dto.PurgeEntryProgressResponse {
	t.Helper()
	// 1. 通过正式地址发送 GET，只带认证和本次请求 ID。
	requestID := uuid.NewString()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Origin", serverPurgeEntryOrigin)
	request.Header.Set("X-Request-ID", requestID)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("正式清理进度请求失败", err)
	}
	defer response.Body.Close()
	headers := response.Header
	if response.StatusCode != status || headers.Get("X-Request-ID") != requestID ||
		headers.Get("Cache-Control") != "no-store" || headers.Get("Content-Type") != "application/json; charset=utf-8" ||
		headers.Get("Access-Control-Allow-Origin") != serverPurgeEntryOrigin ||
		(headers.Get("WWW-Authenticate") == `Bearer realm="cloudnest"`) != (status == http.StatusUnauthorized) ||
		(status != http.StatusUnauthorized && headers.Get("Pragma") != "no-cache") {
		t.Fatalf("正式进度状态 %d，期望 %d，响应头 %v", response.StatusCode, status, headers)
	}

	// 2. 核对公开正文和请求 ID，错误只返回约定的信息。
	decoder := json.NewDecoder(response.Body)
	if status == http.StatusOK {
		var result dto.PurgeEntryProgressResponse
		if err := decoder.Decode(&result); err != nil || result.RequestID != requestID {
			t.Fatal("正式进度成功正文不符", result, err)
		}
		return result
	}
	var failure dto.ErrorResponse
	if err := decoder.Decode(&failure); err != nil || failure.RequestID != requestID || failure.Error.Code != code ||
		(status == http.StatusBadRequest && (len(failure.Error.Details) != 1 || failure.Error.Details["field"] != "id")) ||
		(status != http.StatusBadRequest && failure.Error.Details != nil) {
		t.Fatal("正式进度错误正文不符", failure, err)
	}
	return dto.PurgeEntryProgressResponse{}
}

func verifyServerPurgeEntryProgress(
	t *testing.T, ctx context.Context, baseURL, token, foreignToken string, accepted dto.PurgeEntryAcceptedResponse,
) dto.PurgeEntryProgressResponse {
	t.Helper()
	// 1. 用 202 给出的地址验证认证、归属和受理后的初始进度。
	requestServerPurgeEntryProgress(t, ctx, baseURL, "/api/v1/trash/0/purge", "", http.StatusUnauthorized, "AUTH_REQUIRED")
	requestServerPurgeEntryProgress(t, ctx, baseURL, "/api/v1/trash/0/purge", token, http.StatusBadRequest, "INVALID_ARGUMENT")
	requestServerPurgeEntryProgress(t, ctx, baseURL, accepted.Data.ProgressURL, foreignToken, http.StatusNotFound, "ENTRY_NOT_FOUND")
	initial := requestServerPurgeEntryProgress(t, ctx, baseURL, accepted.Data.ProgressURL, token, http.StatusOK, "")
	want := dto.PurgeEntryProgressData{EntryID: accepted.Data.EntryID, Status: "PURGING", RemainingCount: 1}
	if initial.Data != want || initial.RequestID == accepted.RequestID {
		t.Fatal("从受理地址查询到的初始进度不符", initial)
	}
	return initial
}

func verifyServerPurgeEntryProgressAfterRestart(
	t *testing.T, ctx context.Context, pool *sql.DB, baseURL, token string, progressPath string, before dto.PurgeEntryProgressResponse,
) {
	t.Helper()
	// 1. 在新实例用同账号另一有效会话查询已保存的清理状态。
	restarted := requestServerPurgeEntryProgress(t, ctx, baseURL, progressPath, token, http.StatusOK, "")
	if restarted.Data != before.Data || restarted.RequestID == before.RequestID {
		t.Fatal("重启后进度数据或本次请求 ID 不符", restarted)
	}

	// 2. 在独占库模拟空目录完成清理，再用原地址读取最新状态。
	result, err := pool.ExecContext(ctx, `UPDATE file_entries
		SET status='PURGED',purged_at=UTC_TIMESTAMP(6),revision=revision+1,modified_at=UTC_TIMESTAMP(6),updated_at=UTC_TIMESTAMP(6)
		WHERE user_id=1 AND id=? AND status='PURGING'`, before.Data.EntryID)
	if err != nil {
		t.Fatal("推进清理进度夹具失败", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		t.Fatal("只应推进清理进度夹具一行", affected, err)
	}
	finished := requestServerPurgeEntryProgress(t, ctx, baseURL, progressPath, token, http.StatusOK, "")
	want := dto.PurgeEntryProgressData{EntryID: before.Data.EntryID, Status: "PURGED", RemainingCount: 0}
	if finished.Data != want || finished.RequestID == restarted.RequestID {
		t.Fatal("正式路径未返回完成状态和本次请求 ID", finished)
	}
}
