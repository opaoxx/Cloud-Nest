package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

func TestRegisterCreateDirectoryRoute(t *testing.T) {
	actor := domain.Actor{UserID: 7, DeviceID: 13, SessionID: 17}
	authorizationCalls, createCalls, listCalls := 0, 0, 0
	authorize := func(_ context.Context, token string) (*domain.Actor, error) {
		authorizationCalls++
		if token != "fixture-access" {
			return nil, domain.ErrInvalidAccessToken
		}
		return &actor, nil
	}
	router := NewRouter(logging.New(io.Discard), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
	RegisterDirectoryPageRoute(router, authorize, func(context.Context, domain.Actor, uint64, string, int) (*domain.DirectoryPageResult, error) {
		listCalls++
		return directoryPageSuccessFixture(), nil
	})
	RegisterCreateDirectoryRoute(router, authorize, func(ctx context.Context, received domain.Actor, parentID uint64, name string, color *string, key string) (json.RawMessage, error) {
		createCalls++
		stored, exists := requestcontext.Actor(ctx)
		if received != actor || !exists || stored != actor || requestcontext.RequestID(ctx) == "" || parentID != 11 || name != "学习资料 🌱" || color == nil || *color != "green" || key != "Request-007" {
			t.Error("正式创建路由未准确传递认证身份、参数或请求 ID")
		}
		return json.Marshal(createDirectorySuccessFixture(t))
	})
	for _, testCase := range []struct {
		name, method, path                       string
		bearer, missingKey, preflight, forbidden bool
		status, auth, creates, lists             int
	}{
		{name: "create", method: "POST", path: "/api/v1/directories", bearer: true, status: 201, auth: 1, creates: 1},
		{name: "missing key", method: "POST", path: "/api/v1/directories", bearer: true, missingKey: true, status: 400, auth: 1},
		{name: "authentication first", method: "POST", path: "/api/v1/directories", missingKey: true, status: 401},
		{name: "forbidden origin", method: "POST", path: "/api/v1/directories", bearer: true, forbidden: true, status: 403},
		{name: "preflight", method: "OPTIONS", path: "/api/v1/directories", preflight: true, status: 204},
		{name: "list coexistence", method: "GET", path: "/api/v1/entries?parent_id=12", bearer: true, status: 200, auth: 1, lists: 1},
		{name: "get unsupported", method: "GET", path: "/api/v1/directories", status: 404},
		{name: "patch unsupported", method: "PATCH", path: "/api/v1/directories", status: 404},
		{name: "head unsupported", method: "HEAD", path: "/api/v1/directories", status: 404},
		{name: "trailing slash", method: "POST", path: "/api/v1/directories/", status: 404},
		{name: "extra segment", method: "POST", path: "/api/v1/directories/42", status: 404},
		{name: "public ping", method: "GET", path: "/ping", status: 200},
		{name: "public live", method: "GET", path: "/health/live", status: 200},
		{name: "public ready", method: "GET", path: "/health/ready", status: 200},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			authorizationCalls, createCalls, listCalls = 0, 0, 0
			body := `{"parent_id":"11","name":"学习资料 🌱","color_key":"green"}`
			if testCase.missingKey {
				body = `{"private-broken":`
			}
			request := httptest.NewRequest(testCase.method, testCase.path, strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			if testCase.bearer {
				request.Header.Set("Authorization", "Bearer fixture-access")
			}
			if !testCase.missingKey {
				request.Header.Set("Idempotency-Key", "Request-007")
			}
			if testCase.forbidden {
				request.Header.Set("Origin", "http://private-origin.example")
			}
			if testCase.preflight {
				request.Header.Set("Origin", "http://127.0.0.1:5173")
				request.Header.Set("Access-Control-Request-Method", "POST")
				request.Header.Set("Access-Control-Request-Headers", "Authorization, Content-Type, Idempotency-Key")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != testCase.status || authorizationCalls != testCase.auth || createCalls != testCase.creates || listCalls != testCase.lists {
				t.Fatal("正式创建路由匹配、认证或调用次数错误", response.Code, authorizationCalls, createCalls, listCalls)
			}
			if testCase.creates == 1 {
				var payload dto.CreateDirectoryResponse
				if json.Unmarshal(response.Body.Bytes(), &payload) != nil || payload.Data.Entry.ID != "42" || payload.RequestID != response.Header().Get("X-Request-ID") || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("正式创建成功响应错误")
				}
			}
		})
	}
}

func TestRegisterCreateDirectoryRouteDependencies(t *testing.T) {
	for _, missing := range []string{"router", "authorization", "create"} {
		t.Run(missing, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("缺少依赖仍完成创建路由注册")
				}
			}()
			var router *gin.Engine
			if missing != "router" {
				router = NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			}
			authorize := func(context.Context, string) (*domain.Actor, error) { return nil, nil }
			create := func(context.Context, domain.Actor, uint64, string, *string, string) (json.RawMessage, error) {
				return nil, nil
			}
			if missing == "authorization" {
				authorize = nil
			}
			if missing == "create" {
				create = nil
			}
			RegisterCreateDirectoryRoute(router, authorize, create)
		})
	}
}
