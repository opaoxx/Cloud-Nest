package http

import (
	"context"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
)

func TestRegisterRefreshRoute(t *testing.T) {
	for _, clientType := range []string{"web", "native"} {
		t.Run(clientType, func(t *testing.T) {
			result := refreshSuccessFixture()
			calls := 0
			origin := "http://127.0.0.1:5173"
			router := NewRouter(logging.New(io.Discard), []string{origin}, func(context.Context) bool { return true })
			RegisterRefreshRoute(router, []string{origin}, func(ctx context.Context, raw string) (*domain.RefreshTokenResult, error) {
				calls++
				if raw != "private-old-refresh" {
					t.Error("正式路由传递了错误原文")
				}
				return result, nil
			}, true)
			request := refreshHandlerRequest(t, clientType, "private-old-refresh")
			request.URL.Path = "/api/v1/auth/refresh"
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != 200 || calls != 1 {
				t.Fatal("正式刷新路径未调用已有处理函数")
			}
			if clientType == "web" {
				cookies := response.Result().Cookies()
				if len(cookies) != 1 || !cookies[0].Secure {
					t.Fatal("正式注册未传递 Cookie 配置")
				}
			}
			for _, path := range []string{"/ping", "/health/live", "/health/ready"} {
				response := httptest.NewRecorder()
				router.ServeHTTP(response, httptest.NewRequest("GET", path, nil))
				if response.Code != 200 || calls != 1 {
					t.Fatal("公共入口触发了刷新服务")
				}
			}
			wrongMethod := httptest.NewRecorder()
			router.ServeHTTP(wrongMethod, httptest.NewRequest("GET", "/api/v1/auth/refresh", nil))
			if wrongMethod.Code != 404 || calls != 1 {
				t.Fatal("错误方法触发了刷新服务")
			}
			preflight := httptest.NewRequest("OPTIONS", "/api/v1/auth/refresh", nil)
			preflight.Header.Set("Origin", origin)
			preflight.Header.Set("Access-Control-Request-Method", "POST")
			preflight.Header.Set("Access-Control-Request-Headers", "Content-Type, X-CloudNest-Client")
			preflightResponse := httptest.NewRecorder()
			router.ServeHTTP(preflightResponse, preflight)
			if preflightResponse.Code != 204 || calls != 1 {
				t.Fatal("刷新预检未按公共 CORS 处理")
			}
		})
	}
}

func TestRegisterRefreshRouteDependencies(t *testing.T) {
	for _, missing := range []string{"router", "service"} {
		t.Run(missing, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("缺失依赖仍完成注册")
				}
			}()
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			refresh := func(context.Context, string) (*domain.RefreshTokenResult, error) { return nil, nil }
			if missing == "router" {
				router = nil
			} else {
				refresh = nil
			}
			RegisterRefreshRoute(router, nil, refresh, false)
		})
	}
}
