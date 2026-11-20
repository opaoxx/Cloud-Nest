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

const serverPurgeRestartRootID = "74202"
const serverPurgeRestartProgressPath = "/api/v1/trash/" + serverPurgeRestartRootID + "/purge"

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
) {
	t.Helper()
	// 1. 用 202 给出的地址验证认证、归属及后台可能已经推进的进度。
	requestServerPurgeEntryProgress(t, ctx, baseURL, "/api/v1/trash/0/purge", "", http.StatusUnauthorized, "AUTH_REQUIRED")
	requestServerPurgeEntryProgress(t, ctx, baseURL, "/api/v1/trash/0/purge", token, http.StatusBadRequest, "INVALID_ARGUMENT")
	requestServerPurgeEntryProgress(t, ctx, baseURL, accepted.Data.ProgressURL, foreignToken, http.StatusNotFound, "ENTRY_NOT_FOUND")
	initial := requestServerPurgeEntryProgress(t, ctx, baseURL, accepted.Data.ProgressURL, token, http.StatusOK, "")
	pending := dto.PurgeEntryProgressData{EntryID: accepted.Data.EntryID, Status: "PURGING", RemainingCount: 1}
	completed := dto.PurgeEntryProgressData{EntryID: accepted.Data.EntryID, Status: "PURGED", RemainingCount: 0}
	if (initial.Data != pending && initial.Data != completed) || initial.RequestID == accepted.RequestID {
		t.Fatal("从受理地址查询到的初始进度不符", initial)
	}
}

func prepareServerPurgeRestartRoot(t *testing.T, ctx context.Context, pool *sql.DB) {
	t.Helper()
	// 1. 在两次启动之间保存一个已受理根目录和它的空子目录。
	if _, err := pool.ExecContext(ctx, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,purge_started_at)
		VALUES(?,1,11,'重启清理根项',CAST('重启清理根项' AS BINARY),'DIR','PURGING',UTC_TIMESTAMP(6)),
		      (74203,1,?,'重启清理子项',CAST('重启清理子项' AS BINARY),'DIR','ACTIVE',NULL)`,
		serverPurgeRestartRootID, serverPurgeRestartRootID); err != nil {
		t.Fatal("准备重启待清理记录失败", err)
	}
}

func waitServerPurgeEntryCompleted(t *testing.T, ctx context.Context, baseURL, progressPath, token, entryID string) {
	t.Helper()
	// 1. 在有限时间内用正式进度路由等待 worker 推进，保留最近一次进度用于诊断。
	pollContext, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	poll := time.NewTicker(100 * time.Millisecond)
	defer poll.Stop()
	want := dto.PurgeEntryProgressData{EntryID: entryID, Status: "PURGED", RemainingCount: 0}
	for {
		progress := requestServerPurgeEntryProgress(t, pollContext, baseURL, progressPath, token, http.StatusOK, "")
		if progress.Data == want {
			return
		}
		if progress.Data.EntryID != entryID || progress.Data.Status != "PURGING" || progress.Data.RemainingCount < 1 {
			t.Fatal("后台推进时返回异常进度", progress.Data)
		}
		select {
		case <-pollContext.Done():
			t.Fatal("永久清理 worker 未在期限内完成", progress.Data, pollContext.Err())
		case <-poll.C:
		}
	}
}
