package http

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
)

func TestLoginValidationHTTPFlow(t *testing.T) {
	// 1. 测试专用 Handler 按读取→校验→后续逻辑执行，不注册生产登录接口。
	for _, field := range []string{"username", "password", "installation_id", "device_name", "platform", "client_type"} {
		for _, operation := range []string{"missing", "null", "wrong type"} {
			t.Run(field+"/"+operation, func(t *testing.T) {
				input := map[string]any{"username": "root", "password": "private-password", "installation_id": "28ee758a-0727-42de-ae7d-3bb33155e148", "device_name": "private-device", "platform": "WEB", "client_type": "web"}
				switch operation {
				case "missing":
					delete(input, field)
				case "null":
					input[field] = nil
				case "wrong type":
					input[field] = 42
				}
				encoded, err := json.Marshal(input)
				if err != nil {
					t.Fatal(err)
				}
				var logs bytes.Buffer
				businessCalled := false
				router := NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
				router.POST("/test-login-validation", func(requestContext *gin.Context) {
					var request dto.LoginRequest
					if !binding.ReadJSON(requestContext, &request) {
						return
					}
					if validationError := request.Validate(); validationError != nil {
						handler.InvalidArgument(requestContext, validationError)
						return
					}
					businessCalled = true
					requestContext.Status(204)
				})
				request := httptest.NewRequest("POST", "/test-login-validation?token=private-query", bytes.NewReader(encoded))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Origin", "http://127.0.0.1:5173")
				request.Header.Set("X-Request-ID", "28EE758A-0727-42DE-AE7D-3BB33155E148")
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)

				// 2. 解析错误的 details=null；字段错误才有 details.field，且不能调用后续业务。
				if response.Code != 400 || businessCalled {
					t.Fatalf("status=%d businessCalled=%v", response.Code, businessCalled)
				}
				var body map[string]any
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				detail, ok := body["error"].(map[string]any)
				if !ok || len(body) != 2 || len(detail) != 3 || detail["code"] != "INVALID_ARGUMENT" || detail["message"] == "" {
					t.Fatal("400 结构不正确")
				}
				if operation == "wrong type" {
					if details, exists := detail["details"]; !exists || details != nil {
						t.Fatal("解析错误不应伪装成字段规则错误")
					}
				} else {
					details, ok := detail["details"].(map[string]any)
					if !ok || len(details) != 1 || details["field"] != field {
						t.Fatal("字段定位错误")
					}
				}
				requestID := response.Header().Get("X-Request-ID")
				if requestID != "28ee758a-0727-42de-ae7d-3bb33155e148" || body["request_id"] != requestID || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Access-Control-Allow-Origin") != "http://127.0.0.1:5173" {
					t.Fatal("ID、缓存或 CORS 丢失")
				}
				var record map[string]any
				if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
					t.Fatal(err)
				}
				if record["level"] != "WARN" || record["status"] != float64(400) || record["request_id"] != requestID {
					t.Fatal("日志关联不正确")
				}
				if strings.Contains(response.Body.String(), "private-") || strings.Contains(logs.String(), "private-") {
					t.Fatal("响应或日志泄漏实际输入")
				}
			})
		}
	}
}

func TestLoginValidationSuccessOverRealHTTP(t *testing.T) {
	// 1. 真实 HTTP 只证明读取和校验通过，不查询账号或签发凭证。
	receivedRequests := make(chan dto.LoginRequest, 1)
	router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
	router.POST("/test-login-validation", func(requestContext *gin.Context) {
		var request dto.LoginRequest
		if !binding.ReadJSON(requestContext, &request) {
			return
		}
		if validationError := request.Validate(); validationError != nil {
			handler.InvalidArgument(requestContext, validationError)
			return
		}
		receivedRequests <- request
		requestContext.Status(204)
	})
	server := httptest.NewServer(router)
	defer server.Close()
	input := `{"username":"root","password":" root ","installation_id":"28ee758a-0727-42de-ae7d-3bb33155e148","device_name":"华为手机","platform":"ANDROID","client_type":"native"}`
	response, err := server.Client().Post(server.URL+"/test-login-validation", "application/json", strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	_, readError := io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != 204 {
		t.Fatalf("真实请求校验状态=%d", response.StatusCode)
	}
	received := <-receivedRequests
	if readError != nil || response.StatusCode != 204 || received.Password != " root " || received.Platform != "ANDROID" || response.Header.Get("X-Request-ID") == "" {
		t.Fatal("真实请求未按预期解析、校验或保留密码")
	}
}
