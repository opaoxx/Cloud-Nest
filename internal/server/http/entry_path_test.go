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
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

func TestEntryPathHTTPFlow(t *testing.T) {
	for _, testCase := range []struct {
		name, path, token, code       string
		status, authCalls, parseCalls int
		entryID                       uint64
	}{
		{name: "target differs from actor identifiers", path: "23", token: "fixture-access", status: 204, authCalls: 1, parseCalls: 1, entryID: 23},
		{name: "root shaped id", path: "11", token: "fixture-access", status: 204, authCalls: 1, parseCalls: 1, entryID: 11},
		{name: "ordinary id", path: "13", token: "fixture-access", status: 204, authCalls: 1, parseCalls: 1, entryID: 13},
		{name: "above javascript safe integer", path: "9007199254740993", token: "fixture-access", status: 204, authCalls: 1, parseCalls: 1, entryID: 9007199254740993},
		{name: "above signed integer", path: "9223372036854775808", token: "fixture-access", status: 204, authCalls: 1, parseCalls: 1, entryID: 9223372036854775808},
		{name: "full unsigned id", path: "18446744073709551615", token: "fixture-access", status: 204, authCalls: 1, parseCalls: 1, entryID: ^uint64(0)},
		{name: "query cannot replace path", path: "13?id=17&user_id=99&note=private-query", token: "fixture-access", status: 204, authCalls: 1, parseCalls: 1, entryID: 13},
		{name: "encoded digits", path: "%31%33", token: "fixture-access", status: 204, authCalls: 1, parseCalls: 1, entryID: 13},
		{name: "double encoded digits", path: "%2531%2533", token: "fixture-access", status: 400, code: "INVALID_ARGUMENT", authCalls: 1, parseCalls: 1},
		{name: "zero", path: "0", token: "fixture-access", status: 400, code: "INVALID_ARGUMENT", authCalls: 1, parseCalls: 1},
		{name: "leading zero", path: "013", token: "fixture-access", status: 400, code: "INVALID_ARGUMENT", authCalls: 1, parseCalls: 1},
		{name: "plus sign", path: "+13", token: "fixture-access", status: 400, code: "INVALID_ARGUMENT", authCalls: 1, parseCalls: 1},
		{name: "encoded sign", path: "%2B13", token: "fixture-access", status: 400, code: "INVALID_ARGUMENT", authCalls: 1, parseCalls: 1},
		{name: "encoded space", path: "%2013", token: "fixture-access", status: 400, code: "INVALID_ARGUMENT", authCalls: 1, parseCalls: 1},
		{name: "overflow", path: "18446744073709551616", token: "fixture-access", status: 400, code: "INVALID_ARGUMENT", authCalls: 1, parseCalls: 1},
		{name: "fixed error message", path: "private-entry-path", token: "fixture-access", status: 400, code: "INVALID_ARGUMENT", authCalls: 1, parseCalls: 1},
		{name: "missing bearer before parsing", path: "private-entry-path", status: 401, code: "AUTH_REQUIRED"},
		{name: "invalid bearer before parsing", path: "private-entry-path", token: "invalid-access", status: 401, code: "AUTH_REQUIRED", authCalls: 1},
		{name: "missing path segment", path: "", token: "fixture-access", status: 404, code: "ROUTE_NOT_FOUND"},
		{name: "extra path segment", path: "13/17", token: "fixture-access", status: 404, code: "ROUTE_NOT_FOUND"},
		{name: "encoded slash", path: "13%2F17", token: "fixture-access", status: 404, code: "ROUTE_NOT_FOUND"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 测试路由先认证，再读取 Gin 已解码的路径参数。
			var logs bytes.Buffer
			actor := domain.Actor{UserID: 7, SessionID: 11, DeviceID: 13}
			authCalls, parseCalls, downstreamCalls := 0, 0, 0
			var accepted *dto.EntryPath
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
			router.GET("/test-entries/:id", protected, func(requestContext *gin.Context) {
				parseCalls++
				stored, exists := requestcontext.Actor(requestContext.Request.Context())
				if !exists || stored != actor {
					t.Error("解析阶段缺少准确认证身份")
				}
				input, fieldError := dto.ParseEntryPath(requestContext.Param("id"))
				if fieldError != nil {
					handler.InvalidArgument(requestContext, fieldError)
					return
				}
				accepted = input
				requestContext.Status(204)
			}, func(*gin.Context) { downstreamCalls++ })
			request := httptest.NewRequest("GET", "/test-entries/"+testCase.path, nil)
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
				if accepted == nil || accepted.EntryID != testCase.entryID || downstreamCalls != 1 || response.Body.Len() != 0 {
					t.Fatal("解析后的目标目录项或后续调用错误")
				}
				return
			}
			var body dto.ErrorResponse
			if json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Error.Code != testCase.code || body.RequestID != response.Header().Get("X-Request-ID") || accepted != nil || downstreamCalls != 0 || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("统一错误、空结果或中止处理错误")
			}
			if testCase.status == 400 && (len(body.Error.Details) != 1 || body.Error.Details["field"] != "id" || body.Error.Message != "目录项 ID 须为 1～18446744073709551615 的标准十进制整数") {
				t.Fatal("路径字段错误内容不准确")
			}
			if testCase.status != 400 && body.Error.Details != nil {
				t.Fatal("认证或路由错误包含额外字段")
			}
			if strings.Contains(response.Body.String(), "private-") || strings.Contains(response.Body.String(), "fixture-access") {
				t.Fatal("错误响应回显原始输入")
			}
		})
	}
}
