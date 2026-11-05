package http

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/security"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
)

// newLoginSourceTestRouter 只在测试中模拟未来入口，不向生产路由添加端点。
func newLoginSourceTestRouter(logger *slog.Logger, authenticate func(context.Context, string, string) (*domain.VerifiedAccount, error)) *gin.Engine {
	allowedOrigins := []string{"http://127.0.0.1:5173"}
	policy := security.NewLoginSourcePolicy(allowedOrigins)
	router := NewRouter(logger, allowedOrigins, func(context.Context) bool { return true })
	router.POST("/test-login-source", func(requestContext *gin.Context) {
		// 1. 解析并校验 DTO。
		var input dto.LoginRequest
		if !binding.ReadJSON(requestContext, &input) {
			return
		}
		if validationError := input.Validate(); validationError != nil {
			handler.InvalidArgument(requestContext, validationError)
			return
		}
		// 2. 来源不合规时立即返回，不能触发账号查询或密码计算。
		if !policy.Allows(requestContext.Request, input.ClientType) {
			handler.ClientSourceForbidden(requestContext)
			return
		}
		// 3. 通过后调用与 CredentialService.Authenticate 同签名的测试替身。
		if _, err := authenticate(requestContext.Request.Context(), input.Username, input.Password); err != nil {
			panic("测试凭证替身不应失败")
		}
		requestContext.Status(204)
	})
	return router
}

func TestLoginSourceHTTPFlow(t *testing.T) {
	for _, testCase := range []struct {
		name, clientType, method, body string
		clients, origins               []string
		preflight                      bool
		status, credentialCalls        int
		code                           string
	}{
		{name: "web allowed", clientType: "web", clients: []string{"web"}, origins: []string{"http://127.0.0.1:5173"}, status: 204, credentialCalls: 1},
		{name: "native allowed", clientType: "native", clients: []string{"native"}, status: 204, credentialCalls: 1},
		{name: "web origin missing", clientType: "web", clients: []string{"web"}, status: 403, code: "CLIENT_SOURCE_FORBIDDEN"},
		{name: "web marker missing", clientType: "web", origins: []string{"http://127.0.0.1:5173"}, status: 403, code: "CLIENT_SOURCE_FORBIDDEN"},
		{name: "native marker missing", clientType: "native", status: 403, code: "CLIENT_SOURCE_FORBIDDEN"},
		{name: "native with allowed origin", clientType: "native", clients: []string{"native"}, origins: []string{"http://127.0.0.1:5173"}, status: 403, code: "CLIENT_SOURCE_FORBIDDEN"},
		{name: "mismatched marker", clientType: "web", clients: []string{"native"}, origins: []string{"http://127.0.0.1:5173"}, status: 403, code: "CLIENT_SOURCE_FORBIDDEN"},
		{name: "duplicate marker", clientType: "web", clients: []string{"web", "web"}, origins: []string{"http://127.0.0.1:5173"}, status: 403, code: "CLIENT_SOURCE_FORBIDDEN"},
		{name: "unknown origin stopped by cors", clientType: "native", clients: []string{"native"}, origins: []string{"https://private-origin.example"}, status: 403, code: "CORS_FORBIDDEN"},
		{name: "duplicate origin stopped by cors", clientType: "web", clients: []string{"web"}, origins: []string{"http://127.0.0.1:5173", "http://127.0.0.1:5173"}, status: 403, code: "CORS_FORBIDDEN"},
		{name: "bad json before source", body: `{"password":42}`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "bad dto before source", body: `{}`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "preflight before actual marker", method: "OPTIONS", origins: []string{"http://127.0.0.1:5173"}, preflight: true, status: 204},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var logs bytes.Buffer
			credentialCalls := 0
			router := newLoginSourceTestRouter(logging.New(&logs), func(_ context.Context, username, password string) (*domain.VerifiedAccount, error) {
				credentialCalls++
				if username != "root" || password != " private-password " {
					t.Error("凭证未按原文传递")
				}
				return &domain.VerifiedAccount{ID: 1}, nil
			})
			body := testCase.body
			if body == "" {
				platform := "WEB"
				if testCase.clientType == "native" {
					platform = "ANDROID"
				}
				encoded, err := json.Marshal(dto.LoginRequest{Username: "root", Password: " private-password ", InstallationID: "28ee758a-0727-42de-ae7d-3bb33155e148", DeviceName: "private-device", Platform: platform, ClientType: testCase.clientType})
				if err != nil {
					t.Fatal(err)
				}
				body = string(encoded)
			}
			method := testCase.method
			if method == "" {
				method = "POST"
			}
			request := httptest.NewRequest(method, "/test-login-source?token=private-query", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Request-ID", "28EE758A-0727-42DE-AE7D-3BB33155E148")
			request.Header.Set("Cookie", "session=private-cookie")
			for _, client := range testCase.clients {
				request.Header.Add(security.ClientHeader, client)
			}
			for _, origin := range testCase.origins {
				request.Header.Add("Origin", origin)
			}
			if testCase.preflight {
				request.Header.Set("Access-Control-Request-Method", "POST")
				request.Header.Set("Access-Control-Request-Headers", "content-type,x-cloudnest-client")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != testCase.status || credentialCalls != testCase.credentialCalls {
				t.Fatalf("status=%d credentialCalls=%d body=%s", response.Code, credentialCalls, response.Body)
			}
			requestID := response.Header().Get("X-Request-ID")
			if requestID != "28ee758a-0727-42de-ae7d-3bb33155e148" {
				t.Fatal("请求 ID 丢失")
			}
			if testCase.code != "" {
				var decoded dto.ErrorResponse
				if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
					t.Fatal(err)
				}
				if decoded.Error.Code != testCase.code || decoded.RequestID != requestID || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("统一错误状态、ID 或缓存规则错误")
				}
				if testCase.code == "CLIENT_SOURCE_FORBIDDEN" && (decoded.Error.Message != "客户端类型或请求来源不被允许" || decoded.Error.Details != nil || !strings.Contains(response.Body.String(), `"details":null`)) {
					t.Fatal("客户端来源拒绝错误不符合契约")
				}
			}
			if len(testCase.origins) == 1 && testCase.origins[0] == "http://127.0.0.1:5173" {
				if response.Header().Get("Access-Control-Allow-Origin") != testCase.origins[0] {
					t.Fatal("允许来源无法读取拒绝结果")
				}
			} else if response.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatal("原生或不允许的来源意外得到 CORS 放行头")
			}
			var record map[string]any
			if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			level := "INFO"
			if response.Code >= 400 {
				level = "WARN"
			}
			if record["request_id"] != requestID || record["status"] != float64(response.Code) || record["level"] != level {
				t.Fatal("完成日志与响应不一致")
			}
			if strings.Contains(logs.String(), "private-") || strings.Contains(response.Body.String(), "private-") {
				t.Fatal("日志或响应泄漏原始输入")
			}
		})
	}
}

func TestLoginSourceOverRealHTTPAndConcurrent(t *testing.T) {
	// 1. 同一生产路由测试服务交错处理 Web、原生、拒绝及预检，检查真实 TCP 与并发隔离。
	var credentialCalls atomic.Int64
	router := newLoginSourceTestRouter(logging.New(io.Discard), func(context.Context, string, string) (*domain.VerifiedAccount, error) {
		credentialCalls.Add(1)
		return &domain.VerifiedAccount{ID: 1}, nil
	})
	server := httptest.NewServer(router)
	defer server.Close()
	type outcome struct {
		status int
		err    error
		want   int
	}
	outcomes := make(chan outcome, 24)
	for requestIndex := range 24 {
		go func(requestIndex int) {
			clientType, platform, marker, origin, method, want := "web", "WEB", "web", "http://127.0.0.1:5173", "POST", 204
			switch requestIndex % 4 {
			case 1:
				clientType, platform, marker, origin = "native", "ANDROID", "native", ""
			case 2:
				marker, want = "native", 403
			case 3:
				method, marker = "OPTIONS", ""
			}
			encoded, err := json.Marshal(dto.LoginRequest{Username: "root", Password: "root", InstallationID: "28ee758a-0727-42de-ae7d-3bb33155e148", DeviceName: "browser", Platform: platform, ClientType: clientType})
			if err != nil {
				outcomes <- outcome{err: err}
				return
			}
			request, err := http.NewRequest(method, server.URL+"/test-login-source", bytes.NewReader(encoded))
			if err != nil {
				outcomes <- outcome{err: err}
				return
			}
			request.Header.Set("Content-Type", "application/json")
			if marker != "" {
				request.Header.Set(security.ClientHeader, marker)
			}
			if origin != "" {
				request.Header.Set("Origin", origin)
			}
			if method == "OPTIONS" {
				request.Header.Set("Access-Control-Request-Method", "POST")
				request.Header.Set("Access-Control-Request-Headers", "content-type,x-cloudnest-client")
			}
			response, err := server.Client().Do(request)
			if err != nil {
				outcomes <- outcome{err: err}
				return
			}
			_, readError := io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			outcomes <- outcome{status: response.StatusCode, err: readError, want: want}
		}(requestIndex)
	}
	for range 24 {
		result := <-outcomes
		if result.err != nil || result.status != result.want {
			t.Fatalf("真实 HTTP 结果错误: %+v", result)
		}
	}
	if credentialCalls.Load() != 12 {
		t.Fatal("来源拒绝或预检执行了凭证查询")
	}
}
