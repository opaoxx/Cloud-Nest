package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

func TestRegisterPullSyncChangesRoute(t *testing.T) {
	const path = "/api/v1/sync/changes/pull"
	const origin = "http://127.0.0.1:5173"
	const access = "Bearer fixture-access"
	const baselineToken = "fixture-baseline.signature"
	const body = `{"baseline_token":"` + baselineToken + `","page_size":2}`
	const nextCursor = "fixture-next.signature"
	actor := domain.Actor{UserID: 7, SessionID: 17, DeviceID: 19}
	for _, scenario := range []struct {
		name, method, path, bearer, body, origin string
		status, authCalls, pullCalls             int
		errorCode                                string
	}{
		{name: "formal POST path", method: http.MethodPost, path: path, bearer: access, body: body, status: 200, authCalls: 1, pullCalls: 1},
		{name: "missing Bearer before malformed body", method: http.MethodPost, path: path, body: "{", status: 401, errorCode: "AUTH_REQUIRED"},
		{name: "malformed authorization", method: http.MethodPost, path: path, bearer: "Basic fixture", body: body, status: 401, errorCode: "AUTH_REQUIRED"},
		{name: "expired Bearer before malformed body", method: http.MethodPost, path: path, bearer: "Bearer expired-fixture-access",
			body: "{", status: 401, authCalls: 1, errorCode: "TOKEN_EXPIRED"},
		{name: "invalid body after authentication", method: http.MethodPost, path: path, bearer: access, body: "{}", status: 400,
			authCalls: 1, errorCode: "INVALID_ARGUMENT"},
		{name: "wrong method", method: http.MethodGet, path: path, bearer: access, body: body, status: 404, errorCode: "ROUTE_NOT_FOUND"},
		{name: "wrong path", method: http.MethodPost, path: "/api/v1/sync/changes", bearer: access, body: body, status: 404, errorCode: "ROUTE_NOT_FOUND"},
		{name: "trailing slash", method: http.MethodPost, path: path + "/", bearer: access, body: body, status: 404, errorCode: "ROUTE_NOT_FOUND"},
		{name: "allowed CORS preflight", method: http.MethodOptions, path: path, origin: origin, status: 204},
		{name: "CORS rejects request before authentication", method: http.MethodPost, path: path, bearer: access, body: body,
			origin: "https://unlisted.invalid", status: 403, errorCode: "CORS_FORBIDDEN"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 在独立路由器注册正式路径，装配可计数的认证与拉取方法。
			authCalls, pullCalls := 0, 0
			router := NewRouter(logging.New(io.Discard), []string{origin}, func(context.Context) bool { return true })
			RegisterPullSyncChangesRoute(router, func(_ context.Context, token string) (*domain.Actor, error) {
				authCalls++
				if token == "expired-fixture-access" {
					return nil, domain.ErrAccessTokenExpired
				}
				if token != "fixture-access" {
					t.Fatal("共享认证收到错误的 Bearer 文本")
				}
				return &actor, nil
			}, func(ctx context.Context, received domain.Actor, baseline, cursor string, pageSize int) (*domain.SyncChangePullResult, error) {
				pullCalls++
				stored, exists := requestcontext.Actor(ctx)
				if authCalls != 1 || !exists || stored != actor || received != actor ||
					baseline != baselineToken || cursor != "" || pageSize != 2 || requestcontext.RequestID(ctx) == "" {
					t.Fatal("拉取方法未收到已认证身份、请求 ID 或解析后的输入")
				}
				return &domain.SyncChangePullResult{HighWaterSequence: 17, NextCursor: nextCursor}, nil
			})

			// 2. 核对路径、方法和认证顺序，再检查响应契约。
			request := httptest.NewRequest(scenario.method, scenario.path, strings.NewReader(scenario.body))
			request.Header.Set("Content-Type", "application/json")
			if scenario.bearer != "" {
				request.Header.Set("Authorization", scenario.bearer)
			}
			if scenario.origin != "" {
				request.Header.Set("Origin", scenario.origin)
			}
			if scenario.method == http.MethodOptions {
				request.Header.Set("Access-Control-Request-Method", "POST")
				request.Header.Set("Access-Control-Request-Headers", "Authorization, Content-Type")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != scenario.status || authCalls != scenario.authCalls || pullCalls != scenario.pullCalls {
				t.Fatalf("路由结果不符，状态 %d，授权 %d 次，拉取 %d 次：%s", response.Code, authCalls, pullCalls, response.Body.String())
			}
			requestID := response.Header().Get("X-Request-ID")
			if requestID == "" {
				t.Fatal("缺少请求 ID")
			}
			switch scenario.status {
			case http.StatusOK:
				var result dto.PullSyncChangesResponse
				if json.Unmarshal(response.Body.Bytes(), &result) != nil || result.RequestID != requestID ||
					result.Data.HighWaterSequence != "17" || result.Data.NextCursor != nextCursor || result.Data.HasMore ||
					result.Data.Events == nil || len(result.Data.Events) != 0 || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("增量拉取响应不符：%s", response.Body.String())
				}
			case http.StatusNoContent:
				if response.Header().Get("Access-Control-Allow-Origin") != origin || response.Body.Len() != 0 {
					t.Fatal("正式路径的预检响应不符")
				}
			default:
				var result dto.ErrorResponse
				if json.Unmarshal(response.Body.Bytes(), &result) != nil || result.RequestID != requestID || result.Error.Code != scenario.errorCode {
					t.Fatalf("错误响应不符：%s", response.Body.String())
				}
			}
		})
	}
}

func TestRegisterPullSyncChangesRouteDependencies(t *testing.T) {
	for _, missing := range []string{"router", "authorization", "pull"} {
		t.Run(missing, func(t *testing.T) {
			// 1. 缺少任一必需依赖时，注册应失败，原路由器不增加接口。
			baseRouter := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			before := len(baseRouter.Routes())
			defer func() {
				if recover() == nil || len(baseRouter.Routes()) != before {
					t.Fatal("缺少依赖时仍修改了路由")
				}
			}()
			router := baseRouter
			authorize := func(context.Context, string) (*domain.Actor, error) { return nil, nil }
			pull := func(context.Context, domain.Actor, string, string, int) (*domain.SyncChangePullResult, error) {
				return nil, nil
			}
			switch missing {
			case "router":
				router = nil
			case "authorization":
				authorize = nil
			case "pull":
				pull = nil
			}
			RegisterPullSyncChangesRoute(router, authorize, pull)
		})
	}
}
