package http

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

func TestUserDevicePathHTTPFlow(t *testing.T) {
	for _, testCase := range []struct {
		name, path, token, code, body, field, expectedName string
		status, authCalls, parseCalls                      int
		deviceID                                           uint64
	}{
		{name: "target differs from actor device", path: "17", token: "fixture-access", status: 204, authCalls: 1, parseCalls: 1, deviceID: 17},
		{name: "ordinary id", path: "13", token: "fixture-access", status: 204, authCalls: 1, parseCalls: 1, deviceID: 13},
		{name: "full unsigned id", path: "18446744073709551615", token: "fixture-access", status: 204, authCalls: 1, parseCalls: 1, deviceID: ^uint64(0)},
		{name: "query cannot replace path", path: "13?id=17&user_id=99&note=private-query", token: "fixture-access", status: 204, authCalls: 1, parseCalls: 1, deviceID: 13},
		{name: "encoded digits", path: "%31%33", token: "fixture-access", status: 204, authCalls: 1, parseCalls: 1, deviceID: 13},
		{name: "double encoded digits", path: "%2531%2533", token: "fixture-access", status: 400, code: "INVALID_ARGUMENT", field: "id", authCalls: 1, parseCalls: 1},
		{name: "zero", path: "0", token: "fixture-access", status: 400, code: "INVALID_ARGUMENT", field: "id", authCalls: 1, parseCalls: 1},
		{name: "leading zero", path: "013", token: "fixture-access", status: 400, code: "INVALID_ARGUMENT", field: "id", authCalls: 1, parseCalls: 1},
		{name: "plus sign", path: "+13", token: "fixture-access", status: 400, code: "INVALID_ARGUMENT", field: "id", authCalls: 1, parseCalls: 1},
		{name: "encoded sign", path: "%2B13", token: "fixture-access", status: 400, code: "INVALID_ARGUMENT", field: "id", authCalls: 1, parseCalls: 1},
		{name: "encoded space", path: "%2013", token: "fixture-access", status: 400, code: "INVALID_ARGUMENT", field: "id", authCalls: 1, parseCalls: 1},
		{name: "overflow", path: "18446744073709551616", token: "fixture-access", status: 400, code: "INVALID_ARGUMENT", field: "id", authCalls: 1, parseCalls: 1},
		{name: "fixed error message", path: "private-device-path", token: "fixture-access", status: 400, code: "INVALID_ARGUMENT", field: "id", authCalls: 1, parseCalls: 1},
		{name: "missing bearer before parsing", path: "private-device-path", status: 401, code: "AUTH_REQUIRED"},
		{name: "invalid bearer before parsing", path: "private-device-path", token: "invalid-access", status: 401, code: "AUTH_REQUIRED", authCalls: 1},
		{name: "padded name preserved", path: "17", token: "fixture-access", body: `{"name":"  Study PC 🎮  "}`, expectedName: "  Study PC 🎮  ", status: 204, authCalls: 1, parseCalls: 1, deviceID: 17},
		{name: "invalid name", path: "17", token: "fixture-access", body: `{"name":"private-name\n"}`, status: 400, code: "INVALID_ARGUMENT", field: "name", authCalls: 1, parseCalls: 1},
		{name: "malformed body", path: "17", token: "fixture-access", body: `{"name":`, status: 400, code: "INVALID_ARGUMENT", authCalls: 1, parseCalls: 1},
		{name: "body cannot select target", path: "17", token: "fixture-access", body: `{"name":"学习电脑","id":"13"}`, status: 400, code: "INVALID_ARGUMENT", authCalls: 1, parseCalls: 1},
		{name: "invalid id precedes malformed body", path: "0", token: "fixture-access", body: `{"name":`, status: 400, code: "INVALID_ARGUMENT", field: "id", authCalls: 1, parseCalls: 1},
		{name: "authentication precedes body", path: "17", body: `{"name":`, status: 401, code: "AUTH_REQUIRED"},
		{name: "missing path segment", path: "", token: "fixture-access", status: 404, code: "ROUTE_NOT_FOUND"},
		{name: "extra path segment", path: "13/17", token: "fixture-access", status: 404, code: "ROUTE_NOT_FOUND"},
		{name: "encoded slash", path: "13%2F17", token: "fixture-access", status: 404, code: "ROUTE_NOT_FOUND"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 测试路由先认证，再读取 Gin 已解码的路径参数。
			var logs bytes.Buffer
			actor := domain.Actor{UserID: 7, SessionID: 11, DeviceID: 13}
			authCalls, parseCalls, downstreamCalls := 0, 0, 0
			var accepted *dto.UserDevicePath
			var acceptedName *dto.RenameDeviceRequest
			router := NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
			protected := middleware.RequireAuthorization(func(ctx context.Context, token string) (*domain.Actor, error) {
				authCalls++
				if requestcontext.RequestID(ctx) == "" {
					t.Error("认证阶段缺少请求 ID")
				}
				if token != "fixture-access" {
					return nil, domain.ErrInvalidAccessToken
				}
				return &actor, nil
			}, handler.AuthorizationFailure)
			router.PATCH("/test-devices/:id", protected, func(requestContext *gin.Context) {
				parseCalls++
				stored, exists := requestcontext.Actor(requestContext.Request.Context())
				if !exists || stored != actor {
					t.Error("解析阶段缺少准确认证身份")
				}
				input, fieldError := dto.ParseUserDevicePath(requestContext.Param("id"))
				if fieldError != nil {
					handler.InvalidArgument(requestContext, fieldError)
					return
				}
				var nameInput dto.RenameDeviceRequest
				if !binding.ReadJSON(requestContext, &nameInput) {
					return
				}
				if fieldError := nameInput.Validate(); fieldError != nil {
					handler.InvalidArgument(requestContext, fieldError)
					return
				}
				acceptedName = &nameInput
				accepted = input
				requestContext.Status(204)
			}, func(*gin.Context) { downstreamCalls++ })
			bodyInput := testCase.body
			if bodyInput == "" {
				bodyInput = `{"name":"学习电脑"}`
			}
			request := httptest.NewRequest("PATCH", "/test-devices/"+testCase.path, strings.NewReader(bodyInput))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Origin", "http://127.0.0.1:5173")
			request.Header.Set("X-Request-ID", "28EE758A-0727-42DE-AE7D-3BB33155E148")
			if testCase.token != "" {
				request.Header.Set("Authorization", "Bearer "+testCase.token)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			// 2. 核对类型化目标、认证顺序和统一字段错误。
			if response.Code != testCase.status || authCalls != testCase.authCalls || parseCalls != testCase.parseCalls {
				t.Fatal("HTTP 状态、认证或解析次数错误", response.Code, authCalls, parseCalls)
			}
			if response.Header().Get("X-Request-ID") != "28ee758a-0727-42de-ae7d-3bb33155e148" || response.Header().Get("Access-Control-Allow-Origin") != "http://127.0.0.1:5173" {
				t.Fatal("请求 ID 或 CORS 头错误")
			}
			if strings.Contains(logs.String(), "fixture-access") || strings.Contains(logs.String(), "invalid-access") || strings.Contains(logs.String(), "private-query") {
				t.Fatal("完成日志包含访问凭证或原始查询")
			}
			if testCase.status == 204 {
				if accepted == nil || acceptedName == nil || accepted.DeviceID != testCase.deviceID || downstreamCalls != 1 || response.Body.Len() != 0 {
					t.Fatal("解析后的目标设备或后续调用错误")
				}
				expectedName := testCase.expectedName
				if expectedName == "" {
					expectedName = "学习电脑"
				}
				if acceptedName.Name != expectedName {
					t.Fatal("名称原文未准确交给后续步骤")
				}
				return
			}
			var body dto.ErrorResponse
			if json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Error.Code != testCase.code || body.RequestID != response.Header().Get("X-Request-ID") || accepted != nil || acceptedName != nil || downstreamCalls != 0 || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("统一错误、空结果或中止处理错误")
			}
			if testCase.field != "" {
				message := "设备 ID 须为 1～18446744073709551615 的标准十进制整数"
				if testCase.field == "name" {
					message = "设备名称须为 1～100 个字符，不能全为空白或包含控制字符"
				}
				if len(body.Error.Details) != 1 || body.Error.Details["field"] != testCase.field || body.Error.Message != message {
					t.Fatal("路径或名称字段错误内容不准确")
				}
			} else if body.Error.Details != nil {
				t.Fatal("非字段错误包含额外字段")
			}
			if strings.Contains(response.Body.String(), "private-") || strings.Contains(response.Body.String(), "fixture-access") {
				t.Fatal("错误响应回显原始输入")
			}
		})
	}
}
