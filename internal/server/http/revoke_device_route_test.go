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

func TestRegisterRevokeDeviceRoute(t *testing.T) {
	actor := domain.Actor{UserID: 7, SessionID: 11, DeviceID: 13}
	authCalls, revokeCalls, listCalls := 0, 0, 0
	var receivedTarget uint64
	authorize := func(context.Context, string) (*domain.Actor, error) {
		authCalls++
		return &actor, nil
	}
	router := NewRouter(logging.New(io.Discard), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
	RegisterUserDevicesRoute(router, authorize, func(context.Context, domain.Actor, uint64, int) (*domain.UserDeviceList, error) {
		listCalls++
		return userDevicesHandlerFixture(), nil
	})
	RegisterRevokeDeviceRoute(router, authorize, func(ctx context.Context, identity domain.Actor, targetID uint64) (*domain.RevokeDeviceResult, error) {
		revokeCalls++
		stored, exists := requestcontext.Actor(ctx)
		if identity != actor || !exists || stored != actor || requestcontext.RequestID(ctx) == "" {
			t.Error("正式撤销路由缺少准确 Actor 或请求 ID")
		}
		receivedTarget = targetID
		return &domain.RevokeDeviceResult{Revoked: true}, nil
	})
	for _, testCase := range []struct {
		name, method, path                            string
		bearer, preflight, forbidden                  bool
		status, authorizations, revocations, listings int
		targetID                                      uint64
	}{
		{name: "missing bearer", method: "DELETE", path: "/api/v1/devices/19", status: 401},
		{name: "another device", method: "DELETE", path: "/api/v1/devices/19", bearer: true, status: 200, authorizations: 1, revocations: 1, targetID: 19},
		{name: "current device", method: "DELETE", path: "/api/v1/devices/13", bearer: true, status: 200, authorizations: 1, revocations: 1, targetID: 13},
		{name: "encoded target", method: "DELETE", path: "/api/v1/devices/%31%39", bearer: true, status: 200, authorizations: 1, revocations: 1, targetID: 19},
		{name: "maximum target", method: "DELETE", path: "/api/v1/devices/18446744073709551615", bearer: true, status: 200, authorizations: 1, revocations: 1, targetID: ^uint64(0)},
		{name: "invalid target", method: "DELETE", path: "/api/v1/devices/0", bearer: true, status: 400, authorizations: 1},
		{name: "authentication first", method: "DELETE", path: "/api/v1/devices/0", status: 401},
		{name: "preflight", method: "OPTIONS", path: "/api/v1/devices/19", preflight: true, status: 204},
		{name: "forbidden origin", method: "DELETE", path: "/api/v1/devices/19", bearer: true, forbidden: true, status: 403},
		{name: "missing segment", method: "DELETE", path: "/api/v1/devices", bearer: true, status: 404},
		{name: "trailing slash", method: "DELETE", path: "/api/v1/devices/19/", bearer: true, status: 404},
		{name: "wrong method", method: "POST", path: "/api/v1/devices/19", bearer: true, status: 404},
		{name: "list remains available", method: "GET", path: "/api/v1/devices", bearer: true, status: 200, authorizations: 1, listings: 1},
		{name: "ping public", method: "GET", path: "/ping", status: 200},
		{name: "live public", method: "GET", path: "/health/live", status: 200},
		{name: "ready public", method: "GET", path: "/health/ready", status: 200},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			authCalls, revokeCalls, listCalls, receivedTarget = 0, 0, 0, 0
			request := httptest.NewRequest(testCase.method, testCase.path, nil)
			if testCase.bearer {
				request.Header.Set("Authorization", "Bearer fixture-access")
			}
			if testCase.preflight {
				request.Header.Set("Origin", "http://127.0.0.1:5173")
				request.Header.Set("Access-Control-Request-Method", "DELETE")
				request.Header.Set("Access-Control-Request-Headers", "Authorization, X-Request-ID")
			}
			if testCase.forbidden {
				request.Header.Set("Origin", "https://other.example")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != testCase.status || authCalls != testCase.authorizations || revokeCalls != testCase.revocations || listCalls != testCase.listings {
				t.Fatal("正式路由状态或调用次数错误", response.Code, authCalls, revokeCalls, listCalls)
			}
			if testCase.revocations == 1 {
				var body dto.RevokeDeviceResponse
				if receivedTarget != testCase.targetID || json.Unmarshal(response.Body.Bytes(), &body) != nil || !body.Data.Revoked ||
					body.RequestID != response.Header().Get("X-Request-ID") || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("正式撤销路径参数或成功响应错误")
				}
			}
		})
	}
}

func TestRegisterRevokeDeviceRouteDependencies(t *testing.T) {
	for _, missing := range []string{"router", "authorization", "revocation"} {
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
			revoke := func(context.Context, domain.Actor, uint64) (*domain.RevokeDeviceResult, error) { return nil, nil }
			if missing == "authorization" {
				authorize = nil
			}
			if missing == "revocation" {
				revoke = nil
			}
			RegisterRevokeDeviceRoute(router, authorize, revoke)
		})
	}
}

// 同一路径按 HTTP 方法分别进入改名或撤销处理链。
func TestRegisterRevokeDeviceRouteCoexistsWithRename(t *testing.T) {
	actor := domain.Actor{UserID: 7, SessionID: 11, DeviceID: 13}
	calls := make([]string, 0, 2)
	router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
	authorize := func(context.Context, string) (*domain.Actor, error) { return &actor, nil }
	RegisterRenameDeviceRoute(router, authorize, func(_ context.Context, identity domain.Actor, targetID uint64, name string) (*domain.UserDevice, error) {
		calls = append(calls, "rename")
		if identity != actor || targetID != 19 || name != "学习设备" {
			t.Error("共存路由的改名参数错误")
		}
		result := renameDeviceSuccessFixture()
		result.ID, result.UserID, result.Name = targetID, identity.UserID, name
		return result, nil
	})
	RegisterRevokeDeviceRoute(router, authorize, func(_ context.Context, identity domain.Actor, targetID uint64) (*domain.RevokeDeviceResult, error) {
		calls = append(calls, "revoke")
		if identity != actor || targetID != 19 {
			t.Error("共存路由的撤销参数错误")
		}
		return &domain.RevokeDeviceResult{Revoked: true}, nil
	})
	for _, method := range []string{"PATCH", "DELETE"} {
		request := httptest.NewRequest(method, "/api/v1/devices/19", strings.NewReader(`{"name":"学习设备"}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer fixture-access")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != 200 {
			t.Fatal("共存路径请求失败", method, response.Code)
		}
	}
	if strings.Join(calls, ",") != "rename,revoke" {
		t.Fatal("请求进入了错误的方法处理链")
	}
}
