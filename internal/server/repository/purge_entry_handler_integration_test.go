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

func TestPurgeEntryHandlerIntegration(t *testing.T) {
	// 1. 在隔离库准备真实会话和回收目录，签发测试访问令牌。
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_, store, actor := trashEntryIdempotencyTestDatabase(t, ctx)
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
	previewSigner, err := service.NewPurgePreviewSigner(jwtConfig.SigningKey)
	if err != nil {
		t.Fatal(err)
	}
	previewVerifier, err := service.NewPurgePreviewVerifier(jwtConfig.SigningKey)
	if err != nil {
		t.Fatal(err)
	}

	// 2. 在测试路由绑定预检快照、用户事务和两条 HTTP 处理链。
	now := time.Now().UTC().Truncate(time.Microsecond)
	currentTime := func() time.Time { return now }
	preview := service.NewPurgePreviewService(func(ctx context.Context, action func(context.Context, service.PurgePreviewReader) error) error {
		return store.WithFileEntrySnapshot(ctx, func(readContext context.Context, readStore *Store) error {
			return action(readContext, readStore)
		})
	}, previewSigner, currentTime)
	purge := service.NewIdempotentPurgeEntryService(
		func(ctx context.Context, identity domain.Actor, action func(context.Context, service.IdempotentPurgeEntryTransaction) error) error {
			return store.WithUserTransaction(ctx, identity, func(txContext context.Context, txStore *Store) error {
				return action(txContext, txStore)
			})
		}, previewVerifier,
		func(entry *domain.FileEntry) (json.RawMessage, error) {
			data, err := dto.NewPurgeEntryAcceptedData(entry)
			if err != nil {
				return nil, err
			}
			return json.Marshal(data)
		},
		func(body []byte) (string, error) {
			data, err := dto.DecodePurgeEntryAcceptedData(body)
			if err != nil {
				return "", err
			}
			return data.EntryID, nil
		}, currentTime,
	)
	authorization := service.NewAuthorizationService(tokenVerifier.Verify, store.FindSessionAuthorization)
	auth := middleware.RequireAuthorization(authorization.Authorize, handler.AuthorizationFailure)
	router := serverhttp.NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
	router.GET("/test-trash/:id/purge-preview", auth, handler.PurgePreview(preview.Preview))
	router.DELETE("/test-trash/:id", auth, handler.PurgeEntry(purge.Begin))

	// 3. 使用 HTTP 预检的版本和凭证确认，验证真实提交和过期后的同键重试。
	previewRequest := httptest.NewRequest(http.MethodGet, "/test-trash/14/purge-preview", nil).WithContext(ctx)
	previewRequest.Header.Set("Authorization", "Bearer "+token.Token)
	previewResponse := httptest.NewRecorder()
	router.ServeHTTP(previewResponse, previewRequest)
	var preflight dto.PurgePreviewResponse
	if err := json.Unmarshal(previewResponse.Body.Bytes(), &preflight); err != nil ||
		previewResponse.Code != http.StatusOK || preflight.Data.EntryID != "14" || preflight.Data.Revision != "1" {
		t.Fatal("未通过 HTTP 取得清理凭证", previewResponse.Code, err)
	}

	const key = "purge-http-14"
	send := func(revision string, expectedStatus int) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodDelete, "/test-trash/14", nil).WithContext(ctx)
		request.Header.Set("Authorization", "Bearer "+token.Token)
		request.Header.Set("Idempotency-Key", key)
		request.Header.Set(dto.PurgeExpectedRevisionHeader, revision)
		request.Header.Set(dto.PurgePreviewDigestHeader, preflight.Data.PreviewDigest)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != expectedStatus || response.Header().Get("X-Request-ID") == "" ||
			response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" {
			t.Fatal("清理确认状态或响应头不符", response.Code, response.Header())
		}
		return response
	}
	readAccepted := func(response *httptest.ResponseRecorder) dto.PurgeEntryAcceptedResponse {
		t.Helper()
		var accepted dto.PurgeEntryAcceptedResponse
		err := json.Unmarshal(response.Body.Bytes(), &accepted)
		want := dto.PurgeEntryAcceptedData{EntryID: "14", Status: "PURGING", ProgressURL: "/api/v1/trash/14/purge"}
		if err != nil || accepted.Data != want || accepted.RequestID != response.Header().Get("X-Request-ID") {
			t.Fatal("HTTP 受理快照或请求 ID 不符", err)
		}
		return accepted
	}
	first := readAccepted(send(preflight.Data.Revision, http.StatusAccepted))
	now = preflight.Data.ExpiresAt.Add(time.Second)
	replay := readAccepted(send(preflight.Data.Revision, http.StatusAccepted))
	if replay.Data != first.Data || replay.RequestID == first.RequestID {
		t.Fatal("过期凭证的同键同参重试应复用受理快照并分配新请求 ID")
	}
	conflict := send("2", http.StatusConflict)
	var failure dto.ErrorResponse
	if err := json.Unmarshal(conflict.Body.Bytes(), &failure); err != nil ||
		failure.Error.Code != "IDEMPOTENCY_CONFLICT" || failure.Error.Details != nil {
		t.Fatal("同键异参应返回幂等冲突", err)
	}

	// 4. 读回目录项与幂等记录，核对受理状态和版本只写入一次。
	entry, err := store.FindUserFileEntry(ctx, actor.UserID, 14)
	if err != nil || entry == nil || entry.Status != "PURGING" || entry.Revision != 2 {
		t.Fatal("首次受理后的目录项状态或版本错误", entry, err)
	}
	record, err := store.FindUserIdempotencyRecord(ctx, actor.UserID, domain.PurgeEntryOperation, key)
	if err != nil || record == nil || record.ResponseStatus != http.StatusAccepted {
		t.Fatal("首次 202 快照未保存", err)
	}
	saved, err := dto.DecodePurgeEntryAcceptedData(record.ResponseBody)
	if err != nil || *saved != first.Data {
		t.Fatal("已保存快照与 HTTP data 不符", err)
	}
}
