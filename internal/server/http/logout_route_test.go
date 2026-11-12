package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/security"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

func TestRegisterLogoutRoute(t *testing.T) {
	for _, clientType := range []string{"web", "native"} {
		t.Run(clientType, func(t *testing.T) {
			actor := domain.Actor{UserID: 7, DeviceID: 13, SessionID: 11}
			authorizeCalls, logoutCalls := 0, 0
			origin := "http://127.0.0.1:5173"
			router := NewRouter(logging.New(io.Discard), []string{origin}, func(context.Context) bool { return true })
			RegisterLogoutRoute(router, []string{origin}, func(ctx context.Context, raw string) (*domain.Actor, error) {
				authorizeCalls++
				if requestcontext.RequestID(ctx) == "" {
					t.Error("授权调用缺少公共请求 ID")
				}
				if raw != "private-access" {
					return nil, domain.ErrInvalidAccessToken
				}
				return &actor, nil
			}, func(ctx context.Context, received domain.Actor) (*domain.LogoutResult, error) {
				logoutCalls++
				stored, exists := requestcontext.Actor(ctx)
				if received != actor || !exists || stored != actor {
					t.Error("正式路由传递了错误身份或未挂载认证")
				}
				return &domain.LogoutResult{Revoked: true}, nil
			}, true)
			request := logoutHandlerRequest(t, clientType)
			request.URL.Path = "/api/v1/auth/logout"
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			var body dto.LogoutResponse
			if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &body) != nil || !body.Data.Revoked || authorizeCalls != 1 || logoutCalls != 1 {
				t.Fatal("正式退出路径未按认证、退出顺序执行")
			}
			cookies := response.Result().Cookies()
			if clientType == "web" {
				if len(cookies) != 1 || !cookies[0].Secure || cookies[0].MaxAge != -1 {
					t.Fatal("正式退出路由未传递 Cookie 配置")
				}
			} else if len(cookies) != 0 {
				t.Fatal("原生模式修改了 Web Cookie")
			}
			for _, path := range []string{"/ping", "/health/live", "/health/ready"} {
				response := httptest.NewRecorder()
				router.ServeHTTP(response, httptest.NewRequest("GET", path, nil))
				if response.Code != 200 || authorizeCalls != 1 || logoutCalls != 1 {
					t.Fatal("公共入口进入了退出认证链")
				}
			}
			for _, path := range []string{"/api/v1/auth/logout", "/api/v1/auth/logout/"} {
				response := httptest.NewRecorder()
				router.ServeHTTP(response, httptest.NewRequest("GET", path, nil))
				if response.Code != 404 || authorizeCalls != 1 || logoutCalls != 1 {
					t.Fatal("错误方法触发了退出认证链")
				}
			}
			preflight := httptest.NewRequest("OPTIONS", "/api/v1/auth/logout", nil)
			preflight.Header.Set("Origin", origin)
			preflight.Header.Set("Access-Control-Request-Method", "POST")
			preflight.Header.Set("Access-Control-Request-Headers", "Authorization, Content-Type, X-CloudNest-Client")
			preflightResponse := httptest.NewRecorder()
			router.ServeHTTP(preflightResponse, preflight)
			if preflightResponse.Code != 204 || authorizeCalls != 1 || logoutCalls != 1 {
				t.Fatal("退出预检未按公共 CORS 处理")
			}
			unauthenticated := logoutHandlerRequest(t, clientType)
			unauthenticated.URL.Path = "/api/v1/auth/logout"
			unauthenticated.Header.Del("Authorization")
			unauthenticatedResponse := httptest.NewRecorder()
			router.ServeHTTP(unauthenticatedResponse, unauthenticated)
			if unauthenticatedResponse.Code != 401 || authorizeCalls != 1 || logoutCalls != 1 {
				t.Fatal("未认证请求进入了退出服务")
			}
			forbidden := logoutHandlerRequest(t, clientType)
			forbidden.URL.Path = "/api/v1/auth/logout"
			forbidden.Header.Del(security.ClientHeader)
			forbiddenResponse := httptest.NewRecorder()
			router.ServeHTTP(forbiddenResponse, forbidden)
			if forbiddenResponse.Code != 403 || authorizeCalls != 2 || logoutCalls != 1 {
				t.Fatal("正式退出路由的来源策略未生效")
			}
		})
	}
}

func TestRegisterLogoutRouteDependencies(t *testing.T) {
	for _, missing := range []string{"router", "authorization", "logout"} {
		t.Run(missing, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("缺少依赖仍完成注册")
				}
			}()
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			authorize := func(context.Context, string) (*domain.Actor, error) { return nil, nil }
			logout := func(context.Context, domain.Actor) (*domain.LogoutResult, error) { return nil, nil }
			switch missing {
			case "router":
				router = nil
			case "authorization":
				authorize = nil
			case "logout":
				logout = nil
			}
			RegisterLogoutRoute(router, nil, authorize, logout, false)
		})
	}
}
