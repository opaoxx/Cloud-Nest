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

func TestUserDevicesHandlerIntegration(t *testing.T) {
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
	devices := service.NewUserDeviceService(store.ListUserDevices)
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
	var historical *domain.LoginDevice
	if err := store.WithLoginUserTransaction(ctx, 1, func(txContext context.Context, txStore *Store) error {
		var err error
		historical, err = txStore.CreateLoginDevice(txContext, uuid.NewString(), "历史设备", "ANDROID")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	installation := uuid.NewString()
	current := createSession(t, 1, installation, "浏览器设备", "WEB")
	newer := createSession(t, 1, uuid.NewString(), "桌面设备", "WINDOWS")
	observer := createSession(t, 1, installation, "浏览器设备", "WEB")
	foreign := createSession(t, 3, installation, "其他账号设备", "ANDROID")
	changedAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	if current.ID == current.DeviceID || current.DeviceID != observer.DeviceID || current.ID == observer.ID {
		t.Fatal("夹具未区分设备与会话身份")
	}
	if _, err := pool.ExecContext(ctx, "UPDATE user_devices SET created_at=?,last_seen_at=NULL,revoked_at=? WHERE id=?", changedAt.Add(-time.Hour), changedAt, historical.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE user_devices SET last_seen_at=?,revoked_at=? WHERE id=?", changedAt, changedAt, newer.DeviceID); err != nil {
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
	newerToken := issue(t, newer)
	var listCalls atomic.Int32
	var logs bytes.Buffer
	router := serverhttp.NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
	router.GET("/api/v1/test-user-devices", middleware.RequireAuthorization(authorization.Authorize, handler.AuthorizationFailure),
		handler.UserDevices(func(ctx context.Context, actor domain.Actor, beforeID uint64, pageSize int) (*domain.UserDeviceList, error) {
			listCalls.Add(1)
			return devices.List(ctx, actor, beforeID, pageSize)
		}))
	server := httptest.NewServer(router)
	defer server.Close()
	client := server.Client()
	client.Timeout = 5 * time.Second
	get := func(t *testing.T, raw, token string, status int, code, field string) dto.UserDeviceListResponse {
		t.Helper()
		request, err := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/v1/test-user-devices", nil)
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
			t.Fatal("设备列表 HTTP 请求失败", err)
		}
		encoded, readError := io.ReadAll(response.Body)
		closeError := response.Body.Close()
		if readError != nil || closeError != nil || response.StatusCode != status || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("X-Request-ID") == "" || len(response.Cookies()) != 0 {
			t.Fatal("列表网络状态、缓存头或读取错误", response.StatusCode, readError, closeError)
		}
		var payload dto.UserDeviceListResponse
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
	checkItems := func(t *testing.T, payload dto.UserDeviceListResponse, identifiers []uint64, currentDeviceID uint64, hasMore bool) {
		t.Helper()
		if len(payload.Data.Items) != len(identifiers) || payload.Data.HasMore != hasMore {
			t.Fatal("列表项数量或分页提示错误")
		}
		for index, item := range payload.Data.Items {
			if item.ID != strconv.FormatUint(identifiers[index], 10) || item.Current != (identifiers[index] == currentDeviceID) || item.CreatedAt.Location() != time.UTC || item.CreatedAt.Nanosecond()%1000 != 0 {
				t.Fatal("设备 ID、当前标记或时间格式错误")
			}
		}
	}
	// 2. 通过真实 HTTP 验证设备分页、当前标记和历史事实。
	t.Run("pagination and historical facts", func(t *testing.T) {
		full := get(t, "user_id=3&device_id=99&note=private-query", currentToken, 200, "", "")
		checkItems(t, full, []uint64{newer.DeviceID, current.DeviceID, historical.ID}, current.DeviceID, false)
		if full.Data.Items[0].LastSeenAt == nil || !full.Data.Items[0].LastSeenAt.Equal(changedAt) ||
			full.Data.Items[0].RevokedAt == nil || !full.Data.Items[0].RevokedAt.Equal(changedAt) ||
			full.Data.Items[1].Name != "浏览器设备" || full.Data.Items[1].Platform != "WEB" || full.Data.Items[1].RevokedAt != nil ||
			full.Data.Items[2].RevokedAt == nil || !full.Data.Items[2].RevokedAt.Equal(changedAt) || full.Data.Items[2].LastSeenAt != nil ||
			!full.Data.Items[2].CreatedAt.Equal(changedAt.Add(-time.Hour)) {
			t.Fatal("设备展示字段、历史事实或可空时间交付错误")
		}
		first := get(t, "limit=1", currentToken, 200, "", "")
		checkItems(t, first, []uint64{newer.DeviceID}, current.DeviceID, true)
		last := get(t, "before_id="+first.Data.Items[0].ID+"&limit=2", currentToken, 200, "", "")
		checkItems(t, last, []uint64{current.DeviceID, historical.ID}, current.DeviceID, false)
		empty := get(t, "before_id="+strconv.FormatUint(historical.ID, 10), currentToken, 200, "", "")
		checkItems(t, empty, []uint64{}, current.DeviceID, false)
	})
	t.Run("user scope and unsigned boundary", func(t *testing.T) {
		other := get(t, "user_id=1", foreignToken, 200, "", "")
		checkItems(t, other, []uint64{foreign.DeviceID}, foreign.DeviceID, false)
		maximum := get(t, "before_id=18446744073709551615&limit=100", currentToken, 200, "", "")
		checkItems(t, maximum, []uint64{newer.DeviceID, current.DeviceID, historical.ID}, current.DeviceID, false)
	})
	// 3. 认证与参数错误均在列表服务前结束。
	t.Run("authorization precedes query and invalid query stops service", func(t *testing.T) {
		before := listCalls.Load()
		get(t, "limit=0", "", 401, "AUTH_REQUIRED", "")
		get(t, "limit=0", "private-invalid-token", 401, "AUTH_REQUIRED", "")
		get(t, "", newerToken, 401, "SESSION_REVOKED", "")
		get(t, "limit=1&limit=2", currentToken, 400, "INVALID_ARGUMENT", "limit")
		get(t, "before_id=%ZZ", currentToken, 400, "INVALID_ARGUMENT", "query")
		get(t, "before_id=18446744073709551616", currentToken, 400, "INVALID_ARGUMENT", "before_id")
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
	// 4. 同设备其他会话继续使用设备身份，设备撤销后关联凭证被拒绝。
	t.Run("revoked session and same device observer", func(t *testing.T) {
		actor, err := authorization.Authorize(ctx, currentToken)
		if err != nil || actor == nil {
			t.Fatal(err)
		}
		if err := store.WithUserTransaction(ctx, *actor, func(txContext context.Context, txStore *Store) error {
			return txStore.RevokeCurrentSession(txContext)
		}); err != nil {
			t.Fatal(err)
		}
		before := listCalls.Load()
		get(t, "", currentToken, 401, "SESSION_REVOKED", "")
		if listCalls.Load() != before {
			t.Fatal("已撤销会话触发了列表服务")
		}
		observed := get(t, "", observerToken, 200, "", "")
		checkItems(t, observed, []uint64{newer.DeviceID, current.DeviceID, historical.ID}, observer.DeviceID, false)
		if len(observed.Data.Items) != 3 || !observed.Data.Items[1].Current || observed.Data.Items[1].RevokedAt != nil {
			t.Fatal("同设备会话未复用设备项与当前设备标记")
		}
	})
	t.Run("revoked current device", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "UPDATE user_devices SET revoked_at=? WHERE id=?", changedAt, current.DeviceID); err != nil {
			t.Fatal(err)
		}
		before := listCalls.Load()
		get(t, "", observerToken, 401, "SESSION_REVOKED", "")
		if listCalls.Load() != before {
			t.Fatal("已撤销设备触发了列表服务")
		}
		other := get(t, "", foreignToken, 200, "", "")
		checkItems(t, other, []uint64{foreign.DeviceID}, foreign.DeviceID, false)
	})
	server.Close()
	for _, secret := range []string{currentToken, observerToken, foreignToken, newerToken, "private-"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("完成日志包含访问凭证或原始查询值")
		}
	}
}
