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

func TestRegisterUserDevicesRoute(t *testing.T) {
	actor := domain.Actor{UserID: 7, SessionID: 11, DeviceID: 13}
	listCalls, authorizeCalls := 0, 0
	var receivedBefore uint64
	var receivedSize int
	router := NewRouter(logging.New(io.Discard), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
	RegisterUserDevicesRoute(router, func(context.Context, string) (*domain.Actor, error) {
		authorizeCalls++
		return &actor, nil
	}, func(ctx context.Context, received domain.Actor, beforeID uint64, pageSize int) (*domain.UserDeviceList, error) {
		listCalls++
		stored, exists := requestcontext.Actor(ctx)
		if received != actor || !exists || stored != actor || requestcontext.RequestID(ctx) == "" {
			t.Error("正式列表路由缺少准确身份或请求 ID")
		}
		receivedBefore, receivedSize = beforeID, pageSize
		return userDevicesHandlerFixture(), nil
	})
	for _, testCase := range []struct {
		name, method, path    string
		bearer, preflight     bool
		status, auth, queries int
		beforeID              uint64
		pageSize              int
	}{
		{name: "missing bearer", method: "GET", path: "/api/v1/devices", status: 401},
		{name: "default page", method: "GET", path: "/api/v1/devices", bearer: true, status: 200, auth: 1, queries: 1, pageSize: 50},
		{name: "next page", method: "GET", path: "/api/v1/devices?before_id=17&limit=2", bearer: true, status: 200, auth: 1, queries: 1, beforeID: 17, pageSize: 2},
		{name: "full unsigned boundary", method: "GET", path: "/api/v1/devices?before_id=18446744073709551615&limit=100", bearer: true, status: 200, auth: 1, queries: 1, beforeID: ^uint64(0), pageSize: 100},
		{name: "invalid query", method: "GET", path: "/api/v1/devices?limit=0", bearer: true, status: 400, auth: 1},
		{name: "authentication before query", method: "GET", path: "/api/v1/devices?limit=0", status: 401},
		{name: "preflight", method: "OPTIONS", path: "/api/v1/devices", preflight: true, status: 204},
		{name: "ping public", method: "GET", path: "/ping", status: 200},
		{name: "live public", method: "GET", path: "/health/live", status: 200},
		{name: "ready public", method: "GET", path: "/health/ready", status: 200},
		{name: "wrong method", method: "POST", path: "/api/v1/devices", status: 404},
		{name: "trailing slash", method: "GET", path: "/api/v1/devices/", status: 404},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			listCalls, authorizeCalls = 0, 0
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
			if response.Code != testCase.status || listCalls != testCase.queries || authorizeCalls != testCase.auth {
				t.Fatal("正式路由状态、授权或查询次数错误", response.Code, authorizeCalls, listCalls)
			}
			if testCase.queries == 1 {
				var body dto.UserDeviceListResponse
				if receivedBefore != testCase.beforeID || receivedSize != testCase.pageSize || json.Unmarshal(response.Body.Bytes(), &body) != nil ||
					len(body.Data.Items) != 1 || !body.Data.Items[0].Current || body.RequestID != response.Header().Get("X-Request-ID") {
					t.Fatal("正式路由的参数或列表响应错误")
				}
			}
		})
	}
}

func TestRegisterUserDevicesRouteDependencies(t *testing.T) {
	for _, missing := range []string{"router", "authorization", "list"} {
		t.Run(missing, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("缺少依赖仍完成注册")
				}
			}()
			var router *gin.Engine
			if missing != "router" {
				router = NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			}
			authorize := func(context.Context, string) (*domain.Actor, error) { return nil, nil }
			list := func(context.Context, domain.Actor, uint64, int) (*domain.UserDeviceList, error) { return nil, nil }
			if missing == "authorization" {
				authorize = nil
			}
			if missing == "list" {
				list = nil
			}
			RegisterUserDevicesRoute(router, authorize, list)
		})
	}
}
