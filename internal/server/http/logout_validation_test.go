package http

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
)

func TestLogoutValidationHTTPFlow(t *testing.T) {
	webBody := `{"client_type":"web"}`
	maximumBody := webBody + strings.Repeat(" ", int(binding.MaxJSONBodyBytes)-len(webBody))
	for _, testCase := range []struct {
		name, body, contentType, encoding string
		status                            int
		code, field, clientType           string
	}{
		{name: "web", body: webBody, status: 204, clientType: "web"},
		{name: "native", body: `{"client_type":"native"}`, status: 204, clientType: "native"},
		{name: "maximum body bytes", body: maximumBody, status: 204, clientType: "web"},
		{name: "missing client", body: `{}`, status: 400, code: "INVALID_ARGUMENT", field: "client_type"},
		{name: "null client", body: `{"client_type":null}`, status: 400, code: "INVALID_ARGUMENT", field: "client_type"},
		{name: "empty client", body: `{"client_type":""}`, status: 400, code: "INVALID_ARGUMENT", field: "client_type"},
		{name: "uppercase client", body: `{"client_type":"WEB"}`, status: 400, code: "INVALID_ARGUMENT", field: "client_type"},
		{name: "padded client", body: `{"client_type":" native "}`, status: 400, code: "INVALID_ARGUMENT", field: "client_type"},
		{name: "unknown client", body: `{"client_type":"private-client"}`, status: 400, code: "INVALID_ARGUMENT", field: "client_type"},
		{name: "numeric client", body: `{"client_type":7}`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "boolean client", body: `{"client_type":true}`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "object client", body: `{"client_type":{}}`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "unknown user", body: `{"client_type":"web","user_id":"private-user"}`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "unknown device", body: `{"client_type":"web","device_id":"private-device"}`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "unknown session", body: `{"client_type":"web","session_id":"private-session"}`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "unknown refresh token", body: `{"client_type":"native","refresh_token":"private-refresh"}`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "unknown field before validation", body: `{"client_type":"other","password":"private-password"}`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "empty body", status: 400, code: "INVALID_ARGUMENT"},
		{name: "null body", body: `null`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "array body", body: `[]`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "malformed json", body: `{"client_type":`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "multiple objects", body: webBody + `{"client_type":"native"}`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "invalid utf8", body: "{\"client_type\":\"\xff\"}", status: 400, code: "INVALID_ARGUMENT"},
		{name: "oversized body", body: maximumBody + " ", status: 413, code: "REQUEST_BODY_TOO_LARGE"},
		{name: "unsupported media type", body: webBody, contentType: "text/plain", status: 415, code: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "unsupported charset", body: webBody, contentType: "application/json; charset=gbk", status: 415, code: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "compressed body", body: webBody, encoding: "gzip", status: 415, code: "UNSUPPORTED_MEDIA_TYPE"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 在测试入口组合既有 JSON 读取器与本轮字段校验。
			var logs bytes.Buffer
			router := NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
			var accepted *dto.LogoutRequest
			router.POST("/test-logout-validation", func(requestContext *gin.Context) {
				var request dto.LogoutRequest
				if !binding.ReadJSON(requestContext, &request) {
					return
				}
				if validationError := request.Validate(); validationError != nil {
					handler.InvalidArgument(requestContext, validationError)
					return
				}
				accepted = &request
				requestContext.Status(204)
			})
			request := httptest.NewRequest("POST", "/test-logout-validation?client_type=private-query", strings.NewReader(testCase.body))
			contentType := testCase.contentType
			if contentType == "" {
				contentType = "application/json"
			}
			request.Header.Set("Content-Type", contentType)
			if testCase.encoding != "" {
				request.Header.Set("Content-Encoding", testCase.encoding)
			}
			request.Header.Set("Origin", "http://127.0.0.1:5173")
			request.Header.Set("X-Request-ID", "28EE758A-0727-42DE-AE7D-3BB33155E148")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			// 2. 检查解析/字段错误、成功原值及公共请求标识。
			if response.Code != testCase.status {
				t.Fatal("请求状态不正确", response.Code)
			}
			if response.Header().Get("X-Request-ID") != "28ee758a-0727-42de-ae7d-3bb33155e148" || response.Header().Get("Access-Control-Allow-Origin") != "http://127.0.0.1:5173" {
				t.Fatal("请求 ID 或 CORS 头不正确")
			}
			if testCase.status == 204 {
				if accepted == nil || accepted.ClientType != testCase.clientType || response.Body.Len() != 0 {
					t.Fatal("读取成功后的字段或响应正文错误")
				}
			} else {
				if accepted != nil {
					t.Fatal("错误输入进入了后续步骤")
				}
				var body dto.ErrorResponse
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Error.Code != testCase.code || body.RequestID != response.Header().Get("X-Request-ID") || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("统一错误结构不正确", err)
				}
				if testCase.field == "" {
					if body.Error.Details != nil {
						t.Fatal("读取错误的 details 应为空")
					}
				} else if len(body.Error.Details) != 1 || body.Error.Details["field"] != testCase.field || body.Error.Message != "客户端类型须为 web 或 native" {
					t.Fatal("固定字段错误提示不正确")
				}
			}

			// 3. 检查响应及日志中的原始输入保护，字段检查使用测试专用入口。
			if strings.Contains(response.Body.String(), "private-") || strings.Contains(logs.String(), "private-") {
				t.Fatal("响应或日志包含原始字段或查询参数")
			}
		})
	}
}
