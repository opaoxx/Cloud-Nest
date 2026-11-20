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

func TestPurgeEntryProgressHandlerIntegration(t *testing.T) {
	// 1. 准备真实清理状态和会话，签发测试访问令牌。
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	if _, err := pool.ExecContext(ctx, `UPDATE file_entries
		SET status='PURGING',purge_started_at='2026-01-02 03:04:05.123456' WHERE user_id=1 AND id=14`); err != nil {
		t.Fatal("准备清理进度夹具失败", err)
	}
	jwtConfig := config.JWTConfig{
		SigningKey: bytes.Repeat([]byte{9}, 32), Issuer: "cloudnest-test", Audience: "cloudnest-api-test",
		AccessTokenTTL: 15 * time.Minute, RefreshTokenTTL: 24 * time.Hour,
	}
	signer, err := service.NewAccessTokenSigner(jwtConfig)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := service.NewAccessTokenVerifier(jwtConfig)
	if err != nil {
		t.Fatal(err)
	}
	token, err := signer.Issue(domain.LoginSession{
		ID: actor.SessionID, UserID: actor.UserID, DeviceID: actor.DeviceID, ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	// 2. 在测试路由绑定真实 Bearer 认证、MySQL 快照和进度处理器。
	authorization := service.NewAuthorizationService(verifier.Verify, store.FindSessionAuthorization)
	progress := service.NewPurgeProgressService(func(requestContext context.Context, read func(context.Context, service.PurgeProgressReader) error) error {
		return store.WithFileEntrySnapshot(requestContext, func(snapshotContext context.Context, snapshotStore *Store) error {
			return read(snapshotContext, snapshotStore)
		})
	})
	router := serverhttp.NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
	router.GET("/test-trash/:id/purge", middleware.RequireAuthorization(authorization.Authorize, handler.AuthorizationFailure), handler.PurgeEntryProgress(progress.Get))
	send := func(entryID string, authenticated bool, status int) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/test-trash/"+entryID+"/purge", nil).WithContext(ctx)
		if authenticated {
			request.Header.Set("Authorization", "Bearer "+token.Token)
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != status || response.Header().Get("X-Request-ID") == "" ||
			response.Header().Get("Cache-Control") != "no-store" || (authenticated && response.Header().Get("Pragma") != "no-cache") {
			t.Fatal("真实进度查询状态或响应头不符", response.Code, response.Header())
		}
		return response
	}
	checkFailure := func(response *httptest.ResponseRecorder, code string) {
		t.Helper()
		var failure dto.ErrorResponse
		if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil || failure.Error.Code != code ||
			failure.Error.Details != nil || failure.RequestID != response.Header().Get("X-Request-ID") {
			t.Fatal("真实进度错误响应不符", failure, err)
		}
	}
	checkProgress := func(status string, count int) {
		t.Helper()
		response := send("14", true, http.StatusOK)
		var result dto.PurgeEntryProgressResponse
		want := dto.PurgeEntryProgressData{EntryID: "14", Status: status, RemainingCount: count}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Data != want ||
			result.RequestID != response.Header().Get("X-Request-ID") {
			t.Fatal("真实进度成功结果不符", result, err)
		}
	}

	// 3. 用已提交的目录项状态验证进度、不可查目标及状态异常。
	checkFailure(send("0", false, http.StatusUnauthorized), "AUTH_REQUIRED")
	checkFailure(send("12", true, http.StatusNotFound), "ENTRY_NOT_FOUND")
	checkFailure(send("21", true, http.StatusNotFound), "ENTRY_NOT_FOUND")
	checkProgress("PURGING", 2)
	for _, statement := range []string{
		`UPDATE file_entries SET status='PURGED',object_id=NULL,purged_at='2026-01-02 03:04:06.123456' WHERE user_id=1 AND id=15`,
		`UPDATE file_entries SET status='PURGED',purged_at='2026-01-02 03:04:07.123456' WHERE user_id=1 AND id=14`,
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("推进测试清理状态失败", err)
		}
	}
	checkProgress("PURGED", 0)
	checkFailure(send("15", true, http.StatusNotFound), "ENTRY_NOT_FOUND")
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET purged_at='2026-01-01' WHERE user_id=1 AND id=14"); err != nil {
		t.Fatal("准备无效完成时间失败", err)
	}
	checkFailure(send("14", true, http.StatusInternalServerError), "INTERNAL_ERROR")
}
