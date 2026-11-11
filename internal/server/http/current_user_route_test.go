package http

import (
	"context"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
)

func TestRegisterCurrentUserRoute(t *testing.T) {
	queries, authorizations := 0, 0
	router := NewRouter(logging.New(io.Discard), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
	RegisterCurrentUserRoute(router, func(context.Context, string) (*domain.Actor, error) {
		authorizations++
		return &domain.Actor{UserID: 7, SessionID: 11, DeviceID: 13}, nil
	}, func(_ context.Context, userID uint64) (*domain.UserProfile, error) {
		queries++
		if userID != 7 {
			t.Error("注册后的处理链传错了用户")
		}
		return &domain.UserProfile{ID: 7, Status: "ACTIVE", RootEntryID: 17, QuotaBytes: 100}, nil
	})
	for _, testCase := range []struct {
		name, method, path string
		token, preflight   bool
		status, calls      int
	}{
		{name: "protected missing token", method: "GET", path: "/api/v1/me", status: 401},
		{name: "protected valid token", method: "GET", path: "/api/v1/me", token: true, status: 200, calls: 1},
		{name: "preflight", method: "OPTIONS", path: "/api/v1/me", preflight: true, status: 204},
		{name: "ping public", method: "GET", path: "/ping", status: 200},
		{name: "live public", method: "GET", path: "/health/live", status: 200},
		{name: "ready public", method: "GET", path: "/health/ready", status: 200},
		{name: "wrong method", method: "POST", path: "/api/v1/me", status: 404},
		{name: "trailing slash", method: "GET", path: "/api/v1/me/", status: 404},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			authorizations, queries = 0, 0
			request := httptest.NewRequest(testCase.method, testCase.path, nil)
			if testCase.token {
				request.Header.Set("Authorization", "Bearer fixture-token")
			}
			if testCase.preflight {
				request.Header.Set("Origin", "http://127.0.0.1:5173")
				request.Header.Set("Access-Control-Request-Method", "GET")
				request.Header.Set("Access-Control-Request-Headers", "authorization")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != testCase.status || authorizations != testCase.calls || queries != testCase.calls {
				t.Fatal("正式路径注册、保护范围或公共入口状态错误")
			}
		})
	}
}

func TestRegisterCurrentUserRouteDependencies(t *testing.T) {
	for _, missing := range []string{"router", "authorization", "profile"} {
		t.Run(missing, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("缺少装配依赖应触发错误")
				}
			}()
			var router *gin.Engine
			if missing != "router" {
				router = NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			}
			authorize := func(context.Context, string) (*domain.Actor, error) { return nil, nil }
			profile := func(context.Context, uint64) (*domain.UserProfile, error) { return nil, nil }
			if missing == "authorization" {
				authorize = nil
			}
			if missing == "profile" {
				profile = nil
			}
			RegisterCurrentUserRoute(router, authorize, profile)
		})
	}
}
