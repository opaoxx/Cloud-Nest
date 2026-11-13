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

func TestRegisterRenameDeviceRoute(t *testing.T) {
	actor := domain.Actor{UserID: 7, SessionID: 11, DeviceID: 13}
	authCalls, renameCalls, listCalls := 0, 0, 0
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
	RegisterRenameDeviceRoute(router, authorize, func(ctx context.Context, identity domain.Actor, targetID uint64, name string) (*domain.UserDevice, error) {
		renameCalls++
		stored, exists := requestcontext.Actor(ctx)
		if identity != actor || !exists || stored != actor || requestcontext.RequestID(ctx) == "" {
			t.Error("正式改名路由缺少准确 Actor 或请求 ID")
		}
		receivedTarget = targetID
		result := renameDeviceSuccessFixture()
		result.ID, result.UserID, result.Name = targetID, identity.UserID, name
		return result, nil
	})
	for _, testCase := range []struct {
		name, method, path, body                  string
		bearer, preflight, forbidden              bool
		status, authorizations, renames, listings int
		targetID                                  uint64
	}{
		{name: "missing bearer", method: "PATCH", path: "/api/v1/devices/19", status: 401},
		{name: "another device", method: "PATCH", path: "/api/v1/devices/19", bearer: true, status: 200, authorizations: 1, renames: 1, targetID: 19},
		{name: "encoded target", method: "PATCH", path: "/api/v1/devices/%31%39", bearer: true, status: 200, authorizations: 1, renames: 1, targetID: 19},
		{name: "maximum target", method: "PATCH", path: "/api/v1/devices/18446744073709551615", bearer: true, status: 200, authorizations: 1, renames: 1, targetID: ^uint64(0)},
		{name: "invalid target", method: "PATCH", path: "/api/v1/devices/0", bearer: true, status: 400, authorizations: 1},
		{name: "authentication first", method: "PATCH", path: "/api/v1/devices/0", status: 401},
		{name: "missing name", method: "PATCH", path: "/api/v1/devices/19", body: `{}`, bearer: true, status: 400, authorizations: 1},
		{name: "unknown owner field", method: "PATCH", path: "/api/v1/devices/19", body: `{"name":"学习电脑","user_id":"99"}`, bearer: true, status: 400, authorizations: 1},
		{name: "preflight", method: "OPTIONS", path: "/api/v1/devices/19", preflight: true, status: 204},
		{name: "forbidden origin", method: "PATCH", path: "/api/v1/devices/19", bearer: true, forbidden: true, status: 403},
		{name: "missing segment", method: "PATCH", path: "/api/v1/devices", bearer: true, status: 404},
		{name: "trailing slash", method: "PATCH", path: "/api/v1/devices/19/", bearer: true, status: 404},
		{name: "wrong method", method: "POST", path: "/api/v1/devices/19", bearer: true, status: 404},
		{name: "list remains available", method: "GET", path: "/api/v1/devices", bearer: true, status: 200, authorizations: 1, listings: 1},
		{name: "ping public", method: "GET", path: "/ping", status: 200},
		{name: "live public", method: "GET", path: "/health/live", status: 200},
		{name: "ready public", method: "GET", path: "/health/ready", status: 200},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			authCalls, renameCalls, listCalls, receivedTarget = 0, 0, 0, 0
			input := testCase.body
			if input == "" {
				input = `{"name":"正式改名设备"}`
			}
			request := httptest.NewRequest(testCase.method, testCase.path, strings.NewReader(input))
			request.Header.Set("Content-Type", "application/json")
			if testCase.bearer {
				request.Header.Set("Authorization", "Bearer fixture-access")
			}
			if testCase.preflight {
				request.Header.Set("Origin", "http://127.0.0.1:5173")
				request.Header.Set("Access-Control-Request-Method", "PATCH")
				request.Header.Set("Access-Control-Request-Headers", "Authorization, X-Request-ID")
			}
			if testCase.forbidden {
				request.Header.Set("Origin", "https://other.example")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != testCase.status || authCalls != testCase.authorizations || renameCalls != testCase.renames || listCalls != testCase.listings {
				t.Fatal("正式路由状态或调用次数错误", response.Code, authCalls, renameCalls, listCalls)
			}
			if testCase.renames == 1 {
				var body dto.RenameDeviceResponse
				if receivedTarget != testCase.targetID || json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Data.Device.Name != "正式改名设备" ||
					body.RequestID != response.Header().Get("X-Request-ID") || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("正式改名路径参数或成功响应错误")
				}
			}
		})
	}
}

func TestRegisterRenameDeviceRouteDependencies(t *testing.T) {
	for _, missing := range []string{"router", "authorization", "rename"} {
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
			rename := func(context.Context, domain.Actor, uint64, string) (*domain.UserDevice, error) { return nil, nil }
			if missing == "authorization" {
				authorize = nil
			}
			if missing == "rename" {
				rename = nil
			}
			RegisterRenameDeviceRoute(router, authorize, rename)
		})
	}
}
