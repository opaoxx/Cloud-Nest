package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
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

func TestUserSessionsHandlerIntegration(t *testing.T) {
	// 1. 在独占库中装配真实 JWT、授权、列表查询和服务。
	pool := loginSessionDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	configuration := config.JWTConfig{SigningKey: bytes.Repeat([]byte{9}, 32), Issuer: "cloudnest-test", Audience: "cloudnest-api-test", AccessTokenTTL: 15 * time.Minute, RefreshTokenTTL: time.Hour}
	signer, err := service.NewAccessTokenSigner(configuration)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := service.NewAccessTokenVerifier(configuration)
	if err != nil {
		t.Fatal(err)
	}
	authorization := service.NewAuthorizationService(verifier.Verify, store.FindSessionAuthorization)
	sessions := service.NewUserSessionService(store.ListUserSessions)
	createSession := func(t *testing.T, userID uint64, installation, name, platform string) domain.LoginSession {
		t.Helper()
		_, hash := service.GenerateRefreshToken()
		var created *domain.LoginSession
		err := store.WithLoginUserTransaction(ctx, userID, func(txContext context.Context, txStore *Store) error {
			device, err := txStore.EnsureLoginDevice(txContext, installation, name, platform)
			if err != nil {
				return err
			}
			created, err = txStore.CreateLoginSession(txContext, device.ID, hash, time.Now().UTC().Add(time.Hour))
			return err
		})
		if err != nil || created == nil {
			t.Fatal("创建列表处理函数夹具失败", err)
		}
		return *created
	}
	installation := uuid.NewString()
	older := createSession(t, 1, installation, "浏览器设备", "WEB")
	current := createSession(t, 1, installation, "浏览器设备", "WEB")
	newer := createSession(t, 1, uuid.NewString(), "桌面设备", "WINDOWS")
	observer := createSession(t, 1, installation, "浏览器设备", "WEB")
	foreign := createSession(t, 3, uuid.NewString(), "其他账号设备", "ANDROID")
	changedAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	if _, err := pool.ExecContext(ctx, "UPDATE auth_sessions SET created_at=?,last_seen_at=?,revoked_at=?,expires_at=? WHERE id=?", changedAt.Add(-time.Hour), changedAt, changedAt, changedAt, older.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE user_devices SET revoked_at=? WHERE id=?", changedAt, newer.DeviceID); err != nil {
		t.Fatal(err)
	}
	issue := func(t *testing.T, session domain.LoginSession) string {
		t.Helper()
		token, err := signer.Issue(session)
		if err != nil {
			t.Fatal(err)
		}
		return token.Token
	}
	currentToken, observerToken, foreignToken := issue(t, current), issue(t, observer), issue(t, foreign)
	var listCalls atomic.Int32
	var logs bytes.Buffer
	router := serverhttp.NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
	router.GET("/api/v1/test-user-sessions", middleware.RequireAuthorization(authorization.Authorize, handler.AuthorizationFailure),
		handler.UserSessions(func(ctx context.Context, actor domain.Actor, beforeID uint64, pageSize int) (*domain.UserSessionList, error) {
			listCalls.Add(1)
			return sessions.List(ctx, actor, beforeID, pageSize)
		}))
	server := httptest.NewServer(router)
	defer server.Close()
	client := server.Client()
	client.Timeout = 5 * time.Second
	get := func(t *testing.T, raw, token string, status int, code, field string) dto.UserSessionListResponse {
		t.Helper()
		request, err := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/v1/test-user-sessions", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.URL.RawQuery = raw
		request.Header.Set("Origin", "http://127.0.0.1:5173")
		request.AddCookie(&http.Cookie{Name: "cloudnest_refresh_token", Value: "private-cookie"})
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal("会话列表 HTTP 请求失败", err)
		}
		encoded, readError := io.ReadAll(response.Body)
		closeError := response.Body.Close()
		if readError != nil || closeError != nil || response.StatusCode != status || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("X-Request-ID") == "" || len(response.Cookies()) != 0 {
			t.Fatal("列表网络状态、缓存头或读取错误", response.StatusCode, readError, closeError)
		}
		var payload dto.UserSessionListResponse
		if status == 200 {
			if json.Unmarshal(encoded, &payload) != nil || payload.Data.Items == nil || payload.RequestID != response.Header.Get("X-Request-ID") || response.Header.Get("Pragma") != "no-cache" {
				t.Fatal("列表成功 JSON 或请求 ID 错误")
			}
		} else {
			var failure dto.ErrorResponse
			if json.Unmarshal(encoded, &failure) != nil || failure.Error.Code != code || failure.RequestID != response.Header.Get("X-Request-ID") {
				t.Fatal("列表错误 JSON 或请求 ID 错误")
			}
			if field == "" {
				if failure.Error.Details != nil {
					t.Fatal("非字段错误 details 应为空")
				}
			} else if len(failure.Error.Details) != 1 || failure.Error.Details["field"] != field {
				t.Fatal("列表查询错误位置不正确")
			}
		}
		return payload
	}
	checkItems := func(t *testing.T, payload dto.UserSessionListResponse, identifiers []uint64, currentSessionID uint64, hasMore bool) {
		t.Helper()
		if len(payload.Data.Items) != len(identifiers) || payload.Data.HasMore != hasMore {
			t.Fatal("列表项数量或分页提示错误")
		}
		for index, item := range payload.Data.Items {
			if item.ID != strconv.FormatUint(identifiers[index], 10) || item.Current != (identifiers[index] == currentSessionID) || item.CreatedAt.Location() != time.UTC || item.CreatedAt.Nanosecond()%1000 != 0 {
				t.Fatal("会话 ID、当前标记或时间格式错误")
			}
		}
	}
	// 2. 通过真实 HTTP 验证分页、当前标记及历史状态。
	t.Run("pagination and historical facts", func(t *testing.T) {
		full := get(t, "user_id=3&note=private-query", currentToken, 200, "", "")
		checkItems(t, full, []uint64{observer.ID, newer.ID, current.ID, older.ID}, current.ID, false)
		if full.Data.Items[1].DeviceRevokedAt == nil || !full.Data.Items[1].DeviceRevokedAt.Equal(changedAt) || full.Data.Items[3].RevokedAt == nil || full.Data.Items[3].LastSeenAt == nil ||
			!full.Data.Items[3].ExpiresAt.Equal(changedAt) || full.Data.Items[2].RevokedAt != nil {
			t.Fatal("历史状态或可空时间未按事实交付")
		}
		first := get(t, "limit=2", currentToken, 200, "", "")
		checkItems(t, first, []uint64{observer.ID, newer.ID}, current.ID, true)
		last := get(t, "before_id="+first.Data.Items[1].ID+"&limit=2", currentToken, 200, "", "")
		checkItems(t, last, []uint64{current.ID, older.ID}, current.ID, false)
		empty := get(t, "before_id="+strconv.FormatUint(older.ID, 10), currentToken, 200, "", "")
		checkItems(t, empty, []uint64{}, current.ID, false)
	})
	t.Run("user scope and unsigned boundary", func(t *testing.T) {
		other := get(t, "user_id=1", foreignToken, 200, "", "")
		checkItems(t, other, []uint64{foreign.ID}, foreign.ID, false)
		maximum := get(t, "before_id=18446744073709551615&limit=100", currentToken, 200, "", "")
		checkItems(t, maximum, []uint64{observer.ID, newer.ID, current.ID, older.ID}, current.ID, false)
	})
	// 3. 认证与参数错误均在列表服务前结束。
	t.Run("authorization precedes query and invalid query stops service", func(t *testing.T) {
		before := listCalls.Load()
		get(t, "limit=0", "", 401, "AUTH_REQUIRED", "")
		get(t, "limit=0", "private-invalid-token", 401, "AUTH_REQUIRED", "")
		get(t, "limit=1&limit=2", currentToken, 400, "INVALID_ARGUMENT", "limit")
		get(t, "before_id=%ZZ", currentToken, 400, "INVALID_ARGUMENT", "query")
		if listCalls.Load() != before {
			t.Fatal("未认证或非法查询触发了列表服务")
		}
	})
	t.Run("disabled account", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "UPDATE users SET status='DISABLED' WHERE id=1"); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if _, err := pool.ExecContext(ctx, "UPDATE users SET status='ACTIVE' WHERE id=1"); err != nil {
				t.Error("恢复测试账号失败", err)
			}
		}()
		before := listCalls.Load()
		get(t, "", currentToken, 403, "ACCOUNT_DISABLED", "")
		if listCalls.Load() != before {
			t.Fatal("禁用账号触发了列表服务")
		}
	})
	// 4. 当前会话撤销后凭证被拒绝，同设备另一会话可查看撤销事实。
	t.Run("revoked current session and independent observer", func(t *testing.T) {
		actor, err := authorization.Authorize(ctx, currentToken)
		if err != nil || actor == nil {
			t.Fatal(err)
		}
		if err := store.WithUserTransaction(ctx, *actor, func(txContext context.Context, txStore *Store) error { return txStore.RevokeCurrentSession(txContext) }); err != nil {
			t.Fatal(err)
		}
		before := listCalls.Load()
		get(t, "", currentToken, 401, "SESSION_REVOKED", "")
		if listCalls.Load() != before {
			t.Fatal("已撤销会话触发了列表服务")
		}
		observed := get(t, "", observerToken, 200, "", "")
		checkItems(t, observed, []uint64{observer.ID, newer.ID, current.ID, older.ID}, observer.ID, false)
		if observed.Data.Items[2].RevokedAt == nil || observed.Data.Items[2].Current || !observed.Data.Items[0].Current {
			t.Fatal("同设备其他会话的当前标记或撤销事实错误")
		}
	})
	server.Close()
	for _, secret := range []string{currentToken, observerToken, foreignToken, "private-"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("完成日志包含访问凭证或原始查询值")
		}
	}
}
