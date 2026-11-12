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

func TestRefreshValidationHTTPFlow(t *testing.T) {
	for _, testCase := range []struct {
		name, body         string
		status             int
		field, receivedRaw string
	}{
		{name: "web omitted token", body: `{"client_type":"web"}`, status: 204},
		{name: "web empty token", body: `{"client_type":"web","refresh_token":""}`, status: 204},
		{name: "web null token", body: `{"client_type":"web","refresh_token":null}`, status: 204},
		{name: "native token", body: `{"client_type":"native","refresh_token":"private-refresh"}`, status: 204, receivedRaw: "private-refresh"},
		{name: "native preserved whitespace", body: `{"client_type":"native","refresh_token":" private-refresh "}`, status: 204, receivedRaw: " private-refresh "},
		{name: "native missing token", body: `{"client_type":"native"}`, status: 400, field: "refresh_token"},
		{name: "native null token", body: `{"client_type":"native","refresh_token":null}`, status: 400, field: "refresh_token"},
		{name: "native empty token", body: `{"client_type":"native","refresh_token":""}`, status: 400, field: "refresh_token"},
		{name: "web body token", body: `{"client_type":"web","refresh_token":"private-refresh"}`, status: 400, field: "refresh_token"},
		{name: "missing client", body: `{"refresh_token":"private-refresh"}`, status: 400, field: "client_type"},
		{name: "null client", body: `{"client_type":null}`, status: 400, field: "client_type"},
		{name: "wrong client", body: `{"client_type":"WEB"}`, status: 400, field: "client_type"},
		{name: "client validation comes first", body: `{"client_type":"other","refresh_token":"private-refresh"}`, status: 400, field: "client_type"},
		{name: "client wrong type", body: `{"client_type":7}`, status: 400},
		{name: "token wrong type", body: `{"client_type":"native","refresh_token":7}`, status: 400},
		{name: "unknown field", body: `{"client_type":"web","password":"private-password"}`, status: 400},
		{name: "multiple objects", body: `{"client_type":"web"}{"client_type":"native"}`, status: 400},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var logs bytes.Buffer
			router := NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
			var accepted *dto.RefreshRequest
			router.POST("/test-refresh-validation", func(requestContext *gin.Context) {
				var request dto.RefreshRequest
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
			request := httptest.NewRequest("POST", "/test-refresh-validation?token=private-query", strings.NewReader(testCase.body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Origin", "http://127.0.0.1:5173")
			request.Header.Set("X-Request-ID", "28EE758A-0727-42DE-AE7D-3BB33155E148")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != testCase.status {
				t.Fatal("请求状态不正确", response.Code)
			}
			if response.Header().Get("X-Request-ID") != "28ee758a-0727-42de-ae7d-3bb33155e148" || response.Header().Get("Access-Control-Allow-Origin") != "http://127.0.0.1:5173" {
				t.Fatal("请求 ID 或 CORS 错误")
			}
			if testCase.status == 204 {
				if accepted == nil || accepted.RefreshToken != testCase.receivedRaw || response.Body.Len() != 0 {
					t.Fatal("成功读取后的原文或正文错误")
				}
			} else {
				if accepted != nil {
					t.Fatal("错误请求进入了后续步骤")
				}
				var body dto.ErrorResponse
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Error.Code != "INVALID_ARGUMENT" || body.RequestID != response.Header().Get("X-Request-ID") || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("统一字段错误结构不正确", err)
				}
				if testCase.field == "" {
					if body.Error.Details != nil {
						t.Fatal("解析错误的 details 应为空")
					}
				} else {
					details := body.Error.Details
					if len(details) != 1 || details["field"] != testCase.field {
						t.Fatal("字段错误定位不正确")
					}
				}
			}
			if strings.Contains(response.Body.String(), "private-") || strings.Contains(logs.String(), "private-") {
				t.Fatal("响应或日志包含凭证及查询原文")
			}
		})
	}
}
