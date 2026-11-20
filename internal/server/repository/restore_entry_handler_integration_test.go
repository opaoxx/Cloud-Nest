package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	serverhttp "github.com/opaoxx/cloud-nest/internal/server/http"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestRestoreEntryHandlerIntegration(t *testing.T) {
	// 1. 准备正式表、真实 JWT 和回退根目录时的同名冲突。
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
	defer cancel()
	pool, store, actor := trashEntryIdempotencyTestDatabase(t, ctx)
	for _, statement := range []string{
		"UPDATE file_entries SET status='TRASHED', deleted_at='2000-01-01 00:00:00.000000' WHERE user_id=1 AND id=15",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(17,1,11,'隐藏.MD',CAST('隐藏.md' AS BINARY),'DIR')",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
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

	// 2. 测试专用入口使用共享认证及当前 Store 的用户事务。
	authorization := service.NewAuthorizationService(verifier.Verify, store.FindSessionAuthorization)
	restore := service.NewIdempotentRestoreEntryService(
		func(ctx context.Context, actor domain.Actor, action func(context.Context, service.IdempotentRestoreEntryTransaction) error) error {
			return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				return action(txContext, txStore)
			})
		},
		func(entry *domain.FileEntry) (json.RawMessage, error) {
			data, err := dto.NewRestoreEntryData(entry)
			if err != nil {
				return nil, err
			}
			return json.Marshal(data)
		},
		func(body json.RawMessage) error {
			_, err := dto.DecodeRestoreEntryData(body)
			return err
		},
	)
	router := serverhttp.NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
	router.POST("/test-trash/:id/restore", middleware.RequireAuthorization(authorization.Authorize, handler.AuthorizationFailure), handler.RestoreEntry(restore.Restore))
	server := httptest.NewServer(router)
	defer server.Close()
	client := server.Client()
	client.Timeout = 5 * time.Second
	send := func(entryID, revision uint64, key string, authorized bool, status int, code string) dto.RestoreEntryResponse {
		t.Helper()
		requestID := uuid.NewString()
		path := fmt.Sprintf("%s/test-trash/%d/restore", server.URL, entryID)
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, path, strings.NewReader(fmt.Sprintf(`{"expected_revision":"%d"}`, revision)))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", key)
		request.Header.Set("X-Request-ID", requestID)
		if authorized {
			request.Header.Set("Authorization", "Bearer "+token.Token)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, readError := io.ReadAll(response.Body)
		closeError := response.Body.Close()
		if readError != nil || closeError != nil || response.StatusCode != status || response.Header.Get("X-Request-ID") != requestID ||
			response.Header.Get("Cache-Control") != "no-store" || (authorized && response.Header.Get("Pragma") != "no-cache") {
			t.Fatal("真实恢复 HTTP 状态或响应头不符", response.StatusCode, status, readError, closeError)
		}
		var success dto.RestoreEntryResponse
		if status == http.StatusOK {
			if err := json.Unmarshal(body, &success); err != nil || success.RequestID != requestID {
				t.Fatal("恢复响应正文异常", err)
			}
		} else {
			var failure dto.ErrorResponse
			if err := json.Unmarshal(body, &failure); err != nil || failure.RequestID != requestID || failure.Error.Code != code ||
				(code == "REVISION_CONFLICT" && failure.Error.Details["current_revision"] != "5") {
				t.Fatal("恢复错误正文异常", failure, err)
			}
		}
		return success
	}

	// 3. 从真实认证到数据库提交验证首次恢复、同键重试和错误映射。
	const key = "restore-http"
	send(0, 0, "", false, 401, "AUTH_REQUIRED")
	send(11, 1, key, true, 403, "ROOT_OPERATION_FORBIDDEN")
	send(21, 1, key, true, 404, "ENTRY_NOT_FOUND")
	send(15, 4, key, true, 409, "REVISION_CONFLICT")
	first := send(15, 5, key, true, 200, "")
	replay := send(15, 5, key, true, 200, "")
	entry, err := store.FindUserFileEntry(ctx, actor.UserID, 15)
	if err != nil || entry.ParentID == nil || *entry.ParentID != 11 || entry.Revision != 6 || entry.Status != "ACTIVE" ||
		entry.Name != "隐藏 (1).md" || first.Data.Entry.Name != entry.Name || first.Data.Entry.ParentID != "11" ||
		first.Data.Entry.Revision != "6" || first.Data != replay.Data || first.RequestID == replay.RequestID {
		t.Fatal("首个恢复结果、重试结果和提交记录不符", entry, first, replay, err)
	}
	send(15, 6, key, true, 409, "IDEMPOTENCY_CONFLICT")
	send(15, 6, "restore-http-new", true, 404, "ENTRY_NOT_FOUND")
}
