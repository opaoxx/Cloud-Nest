package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

func TestRegisterDirectoryPageRoute(t *testing.T) {
	actor := domain.Actor{UserID: 7, SessionID: 17, DeviceID: 19}
	authorizeCalls, listCalls := 0, 0
	var receivedParent uint64
	var receivedCursor string
	var receivedSize int
	router := NewRouter(logging.New(io.Discard), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
	RegisterDirectoryPageRoute(router, func(_ context.Context, token string) (*domain.Actor, error) {
		authorizeCalls++
		if token != "fixture-access" {
			t.Error("正式目录路由未准确传递 Bearer")
		}
		return &actor, nil
	}, func(ctx context.Context, received domain.Actor, parentID uint64, cursor string, pageSize int) (*domain.DirectoryPageResult, error) {
		listCalls++
		stored, exists := requestcontext.Actor(ctx)
		if received != actor || !exists || stored != actor || requestcontext.RequestID(ctx) == "" {
			t.Error("正式目录路由缺少准确身份或请求 ID")
		}
		receivedParent, receivedCursor, receivedSize = parentID, cursor, pageSize
		result := directoryPageSuccessFixture()
		result.Path.Entries[len(result.Path.Entries)-1].ID = parentID
		result.Entries[0].ParentID = &parentID
		return result, nil
	})
	// 同时注册既有静态业务路径，核对新增路由和公共接口的匹配边界。
	RegisterUserDevicesRoute(router, func(context.Context, string) (*domain.Actor, error) {
		return &actor, nil
	}, func(context.Context, domain.Actor, uint64, int) (*domain.UserDeviceList, error) {
		return userDevicesHandlerFixture(), nil
	})
	for _, testCase := range []struct {
		name, method, path, cursor string
		bearer, preflight          bool
		status, auth, queries      int
		parentID                   uint64
		pageSize                   int
	}{
		{name: "missing bearer before query", method: "GET", path: "/api/v1/entries?parent_id=0", status: 401},
		{name: "default page", method: "GET", path: "/api/v1/entries?parent_id=12", bearer: true, status: 200, auth: 1, queries: 1, parentID: 12, pageSize: 50},
		{name: "next page", method: "GET", path: "/api/v1/entries?parent_id=12&cursor=fixture%2Bsigned&limit=2", bearer: true, status: 200, auth: 1, queries: 1, parentID: 12, pageSize: 2, cursor: "fixture+signed"},
		{name: "full unsigned boundary", method: "GET", path: "/api/v1/entries?parent_id=18446744073709551615&limit=100", bearer: true, status: 200, auth: 1, queries: 1, parentID: ^uint64(0), pageSize: 100},
		{name: "missing parent", method: "GET", path: "/api/v1/entries", bearer: true, status: 400, auth: 1},
		{name: "invalid limit", method: "GET", path: "/api/v1/entries?parent_id=12&limit=0", bearer: true, status: 400, auth: 1},
		{name: "preflight", method: "OPTIONS", path: "/api/v1/entries", preflight: true, status: 204},
		{name: "existing devices", method: "GET", path: "/api/v1/devices", bearer: true, status: 200},
		{name: "ping public", method: "GET", path: "/ping", status: 200},
		{name: "live public", method: "GET", path: "/health/live", status: 200},
		{name: "ready public", method: "GET", path: "/health/ready", status: 200},
		{name: "wrong method", method: "POST", path: "/api/v1/entries", status: 404},
		{name: "trailing slash", method: "GET", path: "/api/v1/entries/", status: 404},
		{name: "unregistered detail", method: "GET", path: "/api/v1/entries/12", status: 404},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			authorizeCalls, listCalls = 0, 0
			request := httptest.NewRequest(testCase.method, testCase.path, nil)
			if testCase.bearer {
				request.Header.Set("Authorization", "Bearer fixture-access")
			}
			if testCase.preflight {
				request.Header.Set("Origin", "http://127.0.0.1:5173")
				request.Header.Set("Access-Control-Request-Method", "GET")
				request.Header.Set("Access-Control-Request-Headers", "Authorization, X-Request-ID")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != testCase.status || authorizeCalls != testCase.auth || listCalls != testCase.queries {
				t.Fatal("正式目录路由状态、认证或查询次数错误", response.Code, authorizeCalls, listCalls)
			}
			if testCase.queries == 1 {
				var body dto.DirectoryPageResponse
				if receivedParent != testCase.parentID || receivedCursor != testCase.cursor || receivedSize != testCase.pageSize ||
					json.Unmarshal(response.Body.Bytes(), &body) != nil || len(body.Data.Items) != 1 ||
					body.RequestID != response.Header().Get("X-Request-ID") || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("正式目录路由参数或成功响应错误")
				}
			}
		})
	}
}

func TestRegisterDirectoryPageRouteDependencies(t *testing.T) {
	for _, missing := range []string{"router", "authorization", "list"} {
		t.Run(missing, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("缺少依赖仍完成目录路由注册")
				}
			}()
			var router *gin.Engine
			if missing != "router" {
				router = NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			}
			authorize := func(context.Context, string) (*domain.Actor, error) { return nil, nil }
			list := func(context.Context, domain.Actor, uint64, string, int) (*domain.DirectoryPageResult, error) {
				return nil, nil
			}
			if missing == "authorization" {
				authorize = nil
			}
			if missing == "list" {
				list = nil
			}
			RegisterDirectoryPageRoute(router, authorize, list)
		})
	}
}
