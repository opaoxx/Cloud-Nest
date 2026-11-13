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

func TestRenameDeviceValidationHTTPFlow(t *testing.T) {
	validBody := `{"name":"学习电脑"}`
	maximumBody := validBody + strings.Repeat(" ", int(binding.MaxJSONBodyBytes)-len(validBody))
	for _, testCase := range []struct {
		name, body, contentType, encoding string
		status                            int
		code, field, deviceName           string
	}{
		{name: "chinese name", body: validBody, status: 204, deviceName: "学习电脑"},
		{name: "padded name", body: `{"name":"  Study PC 🎮  "}`, status: 204, deviceName: "  Study PC 🎮  "},
		{name: "maximum emoji", body: `{"name":"` + strings.Repeat("🎮", 100) + `"}`, status: 204, deviceName: strings.Repeat("🎮", 100)},
		{name: "escaped unicode", body: `{"name":"\u5b66\u4e60"}`, status: 204, deviceName: "学习"},
		{name: "combining characters", body: `{"name":"Cafe\u0301"}`, status: 204, deviceName: "Cafe\u0301"},
		{name: "maximum body bytes", body: maximumBody, status: 204, deviceName: "学习电脑"},
		{name: "missing name", body: `{}`, status: 400, code: "INVALID_ARGUMENT", field: "name"},
		{name: "null name", body: `{"name":null}`, status: 400, code: "INVALID_ARGUMENT", field: "name"},
		{name: "empty name", body: `{"name":""}`, status: 400, code: "INVALID_ARGUMENT", field: "name"},
		{name: "unicode whitespace", body: `{"name":" \u00a0\u3000 "}`, status: 400, code: "INVALID_ARGUMENT", field: "name"},
		{name: "excessive name", body: `{"name":"` + strings.Repeat("机", 101) + `"}`, status: 400, code: "INVALID_ARGUMENT", field: "name"},
		{name: "escaped control", body: `{"name":"private-name\n"}`, status: 400, code: "INVALID_ARGUMENT", field: "name"},
		{name: "escaped null byte", body: `{"name":"private-name\u0000"}`, status: 400, code: "INVALID_ARGUMENT", field: "name"},
		{name: "numeric name", body: `{"name":7}`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "boolean name", body: `{"name":true}`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "object name", body: `{"name":{}}`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "array name", body: `{"name":[]}`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "unknown user", body: `{"name":"学习电脑","user_id":"private-user"}`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "unknown device", body: `{"name":"学习电脑","device_id":"private-device"}`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "unknown platform", body: `{"name":"学习电脑","platform":"private-platform"}`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "unknown login field", body: `{"device_name":"private-device"}`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "unknown field before validation", body: `{"name":"","password":"private-password"}`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "empty body", status: 400, code: "INVALID_ARGUMENT"},
		{name: "null body", body: `null`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "array body", body: `[]`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "malformed json", body: `{"name":`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "multiple objects", body: validBody + validBody, status: 400, code: "INVALID_ARGUMENT"},
		{name: "invalid utf8", body: "{\"name\":\"\xff\"}", status: 400, code: "INVALID_ARGUMENT"},
		{name: "oversized body", body: maximumBody + " ", status: 413, code: "REQUEST_BODY_TOO_LARGE"},
		{name: "unsupported media type", body: validBody, contentType: "text/plain", status: 415, code: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "unsupported charset", body: validBody, contentType: "application/json; charset=gbk", status: 415, code: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "compressed body", body: validBody, encoding: "gzip", status: 415, code: "UNSUPPORTED_MEDIA_TYPE"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 在测试入口组合既有 JSON 读取器与本轮字段校验。
			var logs bytes.Buffer
			router := NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
			var accepted *dto.RenameDeviceRequest
			router.PATCH("/test-rename-device-validation", func(requestContext *gin.Context) {
				var request dto.RenameDeviceRequest
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
			request := httptest.NewRequest("PATCH", "/test-rename-device-validation?name=private-query", strings.NewReader(testCase.body))
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
				if accepted == nil || accepted.Name != testCase.deviceName || response.Body.Len() != 0 {
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
				} else if len(body.Error.Details) != 1 || body.Error.Details["field"] != testCase.field || body.Error.Message != "设备名称须为 1～100 个字符，不能全为空白或包含控制字符" {
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
