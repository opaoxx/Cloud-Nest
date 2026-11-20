package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func requestServerPurgePreview(t *testing.T, ctx context.Context, baseURL, token, entryID string, status int, code string) dto.PurgePreviewResponse {
	t.Helper()
	// 1. 向实际启动的服务发送预检请求，使用独立的请求 ID。
	const origin = "http://127.0.0.1:5173"
	requestID := uuid.NewString()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/v1/trash/"+entryID+"/purge-preview", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Request-ID", strings.ToUpper(requestID))
	request.Header.Set("Origin", origin)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	earliestExpiry := time.Now().UTC().Truncate(time.Microsecond).Add(5 * time.Minute)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("正式预检请求失败", err)
	}
	defer response.Body.Close()
	header := response.Header
	if response.StatusCode != status || header.Get("X-Request-ID") != requestID ||
		header.Get("Content-Type") != "application/json; charset=utf-8" || header.Get("Cache-Control") != "no-store" ||
		header.Get("Access-Control-Allow-Origin") != origin ||
		(status != http.StatusUnauthorized && header.Get("Pragma") != "no-cache") {
		t.Fatalf("正式预检响应状态 %d，期望 %d，响应头 %v", response.StatusCode, status, header)
	}

	// 2. 校验公开正文、当次请求关联及五分钟截止时间。
	decoder := json.NewDecoder(response.Body)
	var result dto.PurgePreviewResponse
	if status == http.StatusOK {
		latestExpiry := time.Now().UTC().Add(5 * time.Minute)
		if err := decoder.Decode(&result); err != nil || result.RequestID != requestID || result.Data.EntryID != entryID ||
			result.Data.ExpiresAt.Before(earliestExpiry) || result.Data.ExpiresAt.After(latestExpiry) {
			t.Fatal("正式预检成功正文或有效期不符", err)
		}
		return result
	}
	var failure dto.ErrorResponse
	if err := decoder.Decode(&failure); err != nil || failure.RequestID != requestID || failure.Error.Code != code ||
		(status == http.StatusBadRequest && (len(failure.Error.Details) != 1 || failure.Error.Details["field"] != "id")) ||
		(status != http.StatusBadRequest && failure.Error.Details != nil) {
		t.Fatal("正式预检错误正文不符", failure.Error, err)
	}
	return result
}

func verifyServerPurgePreview(t *testing.T, ctx context.Context, baseURL, token, foreignToken string) dto.PurgePreviewResponse {
	t.Helper()
	// 1. 复用正式回收和回收站测试夹具，验证身份、路径和目标可见性。
	requestServerPurgePreview(t, ctx, baseURL, "", "0", 401, "AUTH_REQUIRED")
	requestServerPurgePreview(t, ctx, baseURL, token, "0", 400, "INVALID_ARGUMENT")
	requestServerPurgePreview(t, ctx, baseURL, token, "11", 403, "ROOT_OPERATION_FORBIDDEN")
	requestServerPurgePreview(t, ctx, baseURL, foreignToken, serverTrashDirectoryID, 404, "ENTRY_NOT_FOUND")
	requestServerPurgePreview(t, ctx, baseURL, token, serverRestoreDirectoryID, 404, "ENTRY_NOT_FOUND")

	// 2. 核对回收目录、活动文件后代和已独立回收的目录一并计入。
	result := requestServerPurgePreview(t, ctx, baseURL, token, serverTrashDirectoryID, 200, "")
	data := result.Data
	if data.Revision != "2" || data.FileCount != 1 || data.DirectoryCount != 2 || data.LogicalBytes != "2048" {
		t.Fatal("正式预检的目标版本或统计不符", data.Revision, data.FileCount, data.DirectoryCount, data.LogicalBytes)
	}
	return result
}

func verifyServerPurgePreviewAfterRestart(t *testing.T, ctx context.Context, baseURL, token string, before dto.PurgePreviewResponse) dto.PurgePreviewResponse {
	t.Helper()
	// 1. 重启后使用持久化密钥，分别验证旧凭证与新预检。
	jwtConfig, err := config.LoadServerJWTConfig()
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := service.NewPurgePreviewVerifier(jwtConfig.SigningKey)
	if err != nil {
		t.Fatal(err)
	}
	after := requestServerPurgePreview(t, ctx, baseURL, token, serverTrashDirectoryID, 200, "")
	if after.RequestID == before.RequestID || after.Data.Revision != before.Data.Revision ||
		after.Data.FileCount != 1 || after.Data.DirectoryCount != 1 || after.Data.LogicalBytes != "2048" {
		t.Fatal("重启后的预检未反映已恢复的子目录")
	}

	// 2. 对照两次响应绑定的范围，核对恢复使子树改变而目标版本保持相同。
	remaining := []domain.PurgePreviewSubtreeEntry{
		{ID: 74002, EntryType: "DIR", Status: "TRASHED", Revision: 2},
		{ID: 74003, EntryType: "FILE", Status: "ACTIVE", Revision: 1, FileSize: 2048},
	}
	previous := append(remaining, domain.PurgePreviewSubtreeEntry{ID: 74005, EntryType: "DIR", Status: "TRASHED", Revision: 1})
	for _, observation := range []struct {
		response dto.PurgePreviewResponse
		entries  []domain.PurgePreviewSubtreeEntry
	}{
		{before, previous},
		{after, remaining},
	} {
		expected, err := service.BuildPurgePreviewSubtreeFingerprint(observation.entries)
		if err != nil {
			t.Fatal(err)
		}
		observed, err := verifier.Verify(1, 74002, observation.response.Data.PreviewDigest, time.Now())
		if err != nil || observed != expected {
			t.Fatal("预检凭证绑定的实际子树指纹不符", err)
		}
	}
	return after
}
