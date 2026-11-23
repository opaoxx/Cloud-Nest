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

func TestRegisterConfirmSyncChangesRoute(t *testing.T) {
	const path = "/api/v1/sync/changes/confirm"
	const origin = "http://127.0.0.1:5173"
	const access = "Bearer fixture-access"
	const cursor = "fixture-confirm.signature"
	const body = `{"cursor":"` + cursor + `"}`
	actor := domain.Actor{UserID: 7, SessionID: 17, DeviceID: 19}

	for _, scenario := range []struct {
		name, method, path, bearer, body, origin string
		status, authCalls, confirmCalls          int
		errorCode                                string
	}{
		{name: "formal POST path", method: http.MethodPost, path: path, bearer: access, body: body,
			status: http.StatusOK, authCalls: 1, confirmCalls: 1},
		{name: "missing Bearer before malformed body", method: http.MethodPost, path: path, body: "{",
			status: http.StatusUnauthorized, errorCode: "AUTH_REQUIRED"},
		{name: "malformed authorization", method: http.MethodPost, path: path, bearer: "Basic fixture", body: body,
			status: http.StatusUnauthorized, errorCode: "AUTH_REQUIRED"},
		{name: "expired Bearer before malformed body", method: http.MethodPost, path: path, bearer: "Bearer expired-fixture-access", body: "{",
			status: http.StatusUnauthorized, authCalls: 1, errorCode: "TOKEN_EXPIRED"},
		{name: "invalid body after authentication", method: http.MethodPost, path: path, bearer: access, body: "{",
			status: http.StatusBadRequest, authCalls: 1, errorCode: "INVALID_ARGUMENT"},
		{name: "wrong method", method: http.MethodGet, path: path, bearer: access, body: body,
			status: http.StatusNotFound, errorCode: "ROUTE_NOT_FOUND"},
		{name: "wrong path", method: http.MethodPost, path: "/api/v1/sync/changes", bearer: access, body: body,
			status: http.StatusNotFound, errorCode: "ROUTE_NOT_FOUND"},
		{name: "trailing slash", method: http.MethodPost, path: path + "/", bearer: access, body: body,
			status: http.StatusNotFound, errorCode: "ROUTE_NOT_FOUND"},
		{name: "allowed CORS preflight", method: http.MethodOptions, path: path, origin: origin, status: http.StatusNoContent},
		{name: "CORS rejects request before authentication", method: http.MethodPost, path: path, bearer: access, body: body,
			origin: "https://unlisted.invalid", status: http.StatusForbidden, errorCode: "CORS_FORBIDDEN"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 在独立路由器绑定可计数的认证与确认方法，捕获它们收到的输入。
			authCalls, confirmCalls := 0, 0
			var confirmationRequestID string
			router := NewRouter(logging.New(io.Discard), []string{origin}, func(context.Context) bool { return true })
			RegisterConfirmSyncChangesRoute(router, func(_ context.Context, token string) (*domain.Actor, error) {
				authCalls++
				if token == "expired-fixture-access" {
					return nil, domain.ErrAccessTokenExpired
				}
				if token != "fixture-access" {
					t.Fatal("共享认证收到错误的 Bearer 文本")
				}
				return &actor, nil
			}, func(ctx context.Context, received domain.Actor, receivedCursor string) (uint64, error) {
				confirmCalls++
				stored, exists := requestcontext.Actor(ctx)
				confirmationRequestID = requestcontext.RequestID(ctx)
				if authCalls != 1 || !exists || stored != actor || received != actor ||
					receivedCursor != cursor || confirmationRequestID == "" {
					t.Fatal("确认方法未收到已认证身份、请求 ID 或游标")
				}
				return 19, nil
			})

			// 2. 发送请求，核对状态码和调用次数，再检查公开响应。
			request := httptest.NewRequest(scenario.method, scenario.path, strings.NewReader(scenario.body))
			request.Header.Set("Content-Type", "application/json")
			if scenario.bearer != "" {
				request.Header.Set("Authorization", scenario.bearer)
			}
			if scenario.origin != "" {
				request.Header.Set("Origin", scenario.origin)
			}
			if scenario.method == http.MethodOptions {
				request.Header.Set("Access-Control-Request-Method", http.MethodPost)
				request.Header.Set("Access-Control-Request-Headers", "Authorization, Content-Type")
			}

			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != scenario.status || authCalls != scenario.authCalls || confirmCalls != scenario.confirmCalls {
				t.Fatalf("路由结果不符，状态 %d，认证 %d 次，确认 %d 次：%s", response.Code, authCalls, confirmCalls, response.Body.String())
			}
			requestID := response.Header().Get("X-Request-ID")
			if requestID == "" {
				t.Fatal("缺少请求 ID")
			}

			switch scenario.status {
			case http.StatusOK:
				var result dto.ConfirmSyncSnapshotBaselineResponse
				if json.Unmarshal(response.Body.Bytes(), &result) != nil || result.RequestID != requestID ||
					confirmationRequestID != requestID || result.Data.Status != "ACTIVE" || result.Data.AckSequence != "19" ||
					response.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("增量确认响应不符：%s", response.Body.String())
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

func TestRegisterConfirmSyncChangesRouteDependencies(t *testing.T) {
	for _, missing := range []string{"router", "authorization", "confirm"} {
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
			confirm := func(context.Context, domain.Actor, string) (uint64, error) { return 0, nil }
			switch missing {
			case "router":
				router = nil
			case "authorization":
				authorize = nil
			case "confirm":
				confirm = nil
			}
			RegisterConfirmSyncChangesRoute(router, authorize, confirm)
		})
	}
}
