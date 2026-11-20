package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	serverhttp "github.com/opaoxx/cloud-nest/internal/server/http"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestPurgePreviewHandlerIntegration(t *testing.T) {
	// 1. 准备真实会话、可预检目标、嵌套回收项和测试签名密钥。
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	if _, err := pool.ExecContext(ctx, `UPDATE file_entries
		SET status='TRASHED', deleted_at='2000-01-01 00:00:00.000000'
		WHERE user_id=1 AND id=15`); err != nil {
		t.Fatal(err)
	}
	jwtConfig := config.JWTConfig{
		SigningKey: bytes.Repeat([]byte{9}, 32), Issuer: "cloudnest-test", Audience: "cloudnest-api-test",
		AccessTokenTTL: 15 * time.Minute, RefreshTokenTTL: 24 * time.Hour,
	}
	tokenSigner, err := service.NewAccessTokenSigner(jwtConfig)
	if err != nil {
		t.Fatal(err)
	}
	tokenVerifier, err := service.NewAccessTokenVerifier(jwtConfig)
	if err != nil {
		t.Fatal(err)
	}
	token, err := tokenSigner.Issue(domain.LoginSession{
		ID: actor.SessionID, UserID: actor.UserID, DeviceID: actor.DeviceID, ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	signer, err := service.NewPurgePreviewSigner(jwtConfig.SigningKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := service.NewPurgePreviewVerifier(jwtConfig.SigningKey)
	if err != nil {
		t.Fatal(err)
	}
	issuedAt := time.Date(2026, 11, 20, 4, 30, 0, 123456000, time.UTC)
	wantFingerprint, err := service.BuildPurgePreviewSubtreeFingerprint([]domain.PurgePreviewSubtreeEntry{
		{ID: 14, EntryType: "DIR", Status: "TRASHED", Revision: 1},
		{ID: 15, EntryType: "FILE", Status: "TRASHED", Revision: 5, FileSize: 2048},
	})
	if err != nil {
		t.Fatal(err)
	}

	// 2. 在测试路由串联 JWT、会话读取、预检快照及成功响应。
	authorization := service.NewAuthorizationService(tokenVerifier.Verify, store.FindSessionAuthorization)
	preview := service.NewPurgePreviewService(func(ctx context.Context, action func(context.Context, service.PurgePreviewReader) error) error {
		return store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, snapshotStore *Store) error {
			return action(snapshotContext, snapshotStore)
		})
	}, signer, func() time.Time { return issuedAt })
	router := serverhttp.NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
	router.GET("/test-trash/:id/purge-preview", middleware.RequireAuthorization(authorization.Authorize, handler.AuthorizationFailure), handler.PurgePreview(preview.Preview))

	// 3. 核对真实认证和数据库结果，预检后目标仍是原版本的回收项。
	for _, scenario := range []struct {
		name, entryID, code string
		authorized          bool
		status              int
	}{
		{"missing credential before invalid ID", "0", "AUTH_REQUIRED", false, 401},
		{"invalid ID", "0", "INVALID_ARGUMENT", true, 400},
		{"true root", "11", "ROOT_OPERATION_FORBIDDEN", true, 403},
		{"active directory", "12", "ENTRY_NOT_FOUND", true, 404},
		{"nested trash", "15", "ENTRY_NOT_FOUND", true, 404},
		{"foreign entry", "21", "ENTRY_NOT_FOUND", true, 404},
		{"missing entry", "99", "ENTRY_NOT_FOUND", true, 404},
		{"directory preview", "14", "", true, 200},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/test-trash/"+scenario.entryID+"/purge-preview", nil).WithContext(ctx)
			if scenario.authorized {
				request.Header.Set("Authorization", "Bearer "+token.Token)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			requestID := response.Header().Get("X-Request-ID")
			if response.Code != scenario.status || requestID == "" || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("真实预检 HTTP 状态或响应头错误", response.Code, response.Header())
			}
			if scenario.status != http.StatusOK {
				var failure dto.ErrorResponse
				if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil || failure.RequestID != requestID ||
					failure.Error.Code != scenario.code || (scenario.status == 400 && failure.Error.Details["field"] != "id") ||
					(scenario.status == 404 && (failure.Error.Details != nil || failure.Error.Message != "目录项不存在")) {
					t.Fatal("真实预检错误响应异常", failure, err)
				}
				return
			}
			var success dto.PurgePreviewResponse
			if err := json.Unmarshal(response.Body.Bytes(), &success); err != nil || success.RequestID != requestID ||
				success.Data.EntryID != "14" || success.Data.Revision != "1" || success.Data.FileCount != 1 ||
				success.Data.DirectoryCount != 1 || success.Data.LogicalBytes != "2048" ||
				!success.Data.ExpiresAt.Equal(issuedAt.Add(5*time.Minute)) {
				t.Fatal("真实预检成功结果不符", err)
			}
			fingerprint, err := verifier.Verify(actor.UserID, 14, success.Data.PreviewDigest, issuedAt.Add(time.Minute))
			if err != nil || fingerprint != wantFingerprint {
				t.Fatal("真实预检凭证与数据库子树不符", err)
			}
			entry, err := store.FindUserFileEntry(ctx, actor.UserID, 14)
			if err != nil || entry == nil || entry.Status != "TRASHED" || entry.Revision != 1 {
				t.Fatal("预检后目标状态或版本不符", entry, err)
			}
		})
	}
}
