package http

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
)

func TestCORSRequestPolicy(t *testing.T) {
	// 1. 经生产路由检查来源、预检和原有错误响应，拒绝分支不能执行接口回调。
	const allowedOrigin = "http://127.0.0.1:5173"
	testCases := []struct {
		name       string
		method     string
		path       string
		origins    []string
		methods    []string
		headers    []string
		wantStatus int
		wantAllow  bool
		wantChecks int
	}{
		{name: "no origin", wantStatus: 200, wantChecks: 1},
		{name: "allowed origin", origins: []string{allowedOrigin}, wantStatus: 200, wantAllow: true, wantChecks: 1},
		{name: "second allowed origin", origins: []string{"http://localhost:5173"}, wantStatus: 200, wantAllow: true, wantChecks: 1},
		{name: "unknown origin", origins: []string{"https://private-unknown.example"}, wantStatus: 403},
		{name: "different port", origins: []string{"http://127.0.0.1:5174"}, wantStatus: 403},
		{name: "different protocol", origins: []string{"https://127.0.0.1:5173"}, wantStatus: 403},
		{name: "suffix attack", origins: []string{"http://127.0.0.1:5173.private-evil.example"}, wantStatus: 403},
		{name: "empty origin", origins: []string{""}, wantStatus: 403},
		{name: "opaque origin", origins: []string{"null"}, wantStatus: 403},
		{name: "wildcard origin", origins: []string{"*"}, wantStatus: 403},
		{name: "repeated origin", origins: []string{allowedOrigin, allowedOrigin}, wantStatus: 403},
		{name: "joined origins", origins: []string{allowedOrigin + ", http://localhost:5173"}, wantStatus: 403},
		{name: "space separated origins", origins: []string{allowedOrigin + " http://localhost:5173"}, wantStatus: 403},
		{name: "root slash not serialized origin", origins: []string{allowedOrigin + "/"}, wantStatus: 403},
		{name: "uppercase not serialized origin", origins: []string{"HTTP://127.0.0.1:5173"}, wantStatus: 403},
		{name: "origin whitespace", origins: []string{" " + allowedOrigin}, wantStatus: 403},
		{name: "preflight minimal", method: "OPTIONS", origins: []string{allowedOrigin}, methods: []string{"GET"}, wantStatus: 204, wantAllow: true},
		{name: "preflight all headers", method: "OPTIONS", origins: []string{allowedOrigin}, methods: []string{"POST"}, headers: []string{"Authorization, Content-Type, Idempotency-Key, X-CloudNest-Client, X-Request-ID"}, wantStatus: 204, wantAllow: true},
		{name: "preflight header case and whitespace", method: "OPTIONS", origins: []string{allowedOrigin}, methods: []string{"PATCH"}, headers: []string{" \tx-request-id\t, CONTENT-TYPE "}, wantStatus: 204, wantAllow: true},
		{name: "preflight multiple header lines", method: "OPTIONS", origins: []string{allowedOrigin}, methods: []string{"PUT"}, headers: []string{"authorization", "x-request-id"}, wantStatus: 204, wantAllow: true},
		{name: "preflight unknown path", method: "OPTIONS", path: "/not-implemented", origins: []string{allowedOrigin}, methods: []string{"DELETE"}, wantStatus: 204, wantAllow: true},
		{name: "preflight head", method: "OPTIONS", origins: []string{allowedOrigin}, methods: []string{"HEAD"}, wantStatus: 204, wantAllow: true},
		{name: "preflight options", method: "OPTIONS", origins: []string{allowedOrigin}, methods: []string{"OPTIONS"}, wantStatus: 204, wantAllow: true},
		{name: "preflight unknown origin", method: "OPTIONS", origins: []string{"https://private-preflight.example"}, methods: []string{"POST"}, wantStatus: 403},
		{name: "preflight connect", method: "OPTIONS", origins: []string{allowedOrigin}, methods: []string{"CONNECT"}, wantStatus: 403},
		{name: "preflight trace", method: "OPTIONS", origins: []string{allowedOrigin}, methods: []string{"TRACE"}, wantStatus: 403},
		{name: "preflight lowercase method", method: "OPTIONS", origins: []string{allowedOrigin}, methods: []string{"post"}, wantStatus: 403},
		{name: "preflight method list", method: "OPTIONS", origins: []string{allowedOrigin}, methods: []string{"GET, POST"}, wantStatus: 403},
		{name: "preflight repeated method", method: "OPTIONS", origins: []string{allowedOrigin}, methods: []string{"GET", "GET"}, wantStatus: 403},
		{name: "preflight empty method", method: "OPTIONS", origins: []string{allowedOrigin}, methods: []string{""}, wantStatus: 403},
		{name: "preflight unknown header", method: "OPTIONS", origins: []string{allowedOrigin}, methods: []string{"POST"}, headers: []string{"x-private-secret"}, wantStatus: 403},
		{name: "preflight wildcard header", method: "OPTIONS", origins: []string{allowedOrigin}, methods: []string{"POST"}, headers: []string{"*"}, wantStatus: 403},
		{name: "preflight empty header", method: "OPTIONS", origins: []string{allowedOrigin}, methods: []string{"POST"}, headers: []string{""}, wantStatus: 403},
		{name: "preflight empty header item", method: "OPTIONS", origins: []string{allowedOrigin}, methods: []string{"POST"}, headers: []string{"authorization,,content-type"}, wantStatus: 403},
		{name: "preflight trailing header separator", method: "OPTIONS", origins: []string{allowedOrigin}, methods: []string{"POST"}, headers: []string{"authorization,"}, wantStatus: 403},
		{name: "ordinary options", method: "OPTIONS", origins: []string{allowedOrigin}, wantStatus: 404, wantAllow: true},
		{name: "options without origin", method: "OPTIONS", methods: []string{"GET"}, wantStatus: 404},
		{name: "allowed not found", path: "/missing", origins: []string{allowedOrigin}, wantStatus: 404, wantAllow: true},
		{name: "allowed wrong method", method: "POST", origins: []string{allowedOrigin}, wantStatus: 404, wantAllow: true},
		{name: "allowed not ready", path: "/test-not-ready", origins: []string{allowedOrigin}, wantStatus: 503, wantAllow: true},
		{name: "allowed recovered panic", path: "/test-panic", origins: []string{allowedOrigin}, wantStatus: 500, wantAllow: true},
		{name: "allowed trailing slash", path: "/health/ready/", origins: []string{allowedOrigin}, wantStatus: 404, wantAllow: true},
		{name: "forbidden trailing slash", path: "/health/ready/", origins: []string{"https://private-slash.example"}, wantStatus: 403},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			var logs bytes.Buffer
			checkCount := 0
			router := NewRouter(logging.New(&logs), []string{allowedOrigin, "http://localhost:5173"}, func(context.Context) bool { checkCount++; return true })
			router.GET("/test-not-ready", func(requestContext *gin.Context) { requestContext.JSON(503, gin.H{"status": "not_ready"}) })
			router.GET("/test-panic", func(*gin.Context) { panic("private-panic") })
			method, path := testCase.method, testCase.path
			if method == "" {
				method = "GET"
			}
			if path == "" {
				path = "/health/ready"
			}
			request := httptest.NewRequest(method, path+"?token=private-query", strings.NewReader("private-body"))
			for _, origin := range testCase.origins {
				request.Header.Add("Origin", origin)
			}
			for _, method := range testCase.methods {
				request.Header.Add("Access-Control-Request-Method", method)
			}
			for _, headers := range testCase.headers {
				request.Header.Add("Access-Control-Request-Headers", headers)
			}
			request.Header.Set("Cookie", "session=private-cookie")
			request.Header.Set("X-Request-ID", "28EE758A-0727-42DE-AE7D-3BB33155E148")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			// 2. 核对状态、是否执行接口、CORS 头和缓存区分；错误和预检不能伪装成接口成功。
			if response.Code != testCase.wantStatus || checkCount != testCase.wantChecks {
				t.Fatalf("status=%d checks=%d body=%s", response.Code, checkCount, response.Body)
			}
			if testCase.wantAllow {
				if response.Header().Get("Access-Control-Allow-Origin") != testCase.origins[0] || response.Header().Get("Access-Control-Allow-Credentials") != "true" {
					t.Fatal("允许来源或凭证头错误")
				}
				if response.Code == 204 {
					if response.Body.Len() != 0 || response.Header().Get("Access-Control-Allow-Methods") != "GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS" || response.Header().Get("Access-Control-Allow-Headers") != "Authorization, Content-Type, Idempotency-Key, X-CloudNest-Client, X-Request-ID" || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Access-Control-Max-Age") != "" {
						t.Fatal("预检正文、允许列表或缓存策略错误")
					}
				} else if response.Header().Get("Access-Control-Expose-Headers") != "X-Request-ID, Retry-After" {
					t.Fatal("浏览器不能读取请求 ID")
				}
			} else {
				for name := range response.Header() {
					if strings.HasPrefix(name, "Access-Control-") {
						t.Fatalf("拒绝或无 Origin 响应意外含有 %s", name)
					}
				}
			}
			vary := strings.Join(response.Header().Values("Vary"), ",")
			if !strings.Contains(vary, "Origin") || (method == "OPTIONS" && (!strings.Contains(vary, "Access-Control-Request-Method") || !strings.Contains(vary, "Access-Control-Request-Headers"))) {
				t.Fatal("Vary 不完整")
			}
			if response.Header().Get("Location") != "" {
				t.Fatal("请求绕过中间件自动重定向")
			}

			// 3. 拒绝响应沿用统一错误结构、null 和 ID；日志不收集来源、凭证或正文。
			requestID := response.Header().Get("X-Request-ID")
			if requestID != "28ee758a-0727-42de-ae7d-3bb33155e148" {
				t.Fatal("请求 ID 丢失")
			}
			if response.Code == 403 {
				var body map[string]any
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				detail, ok := body["error"].(map[string]any)
				if !ok || len(body) != 2 || len(detail) != 3 || detail["code"] != "CORS_FORBIDDEN" || detail["message"] != "请求来源或跨域预检不被允许" || body["request_id"] != requestID {
					t.Fatal("403 结构错误")
				}
				if value, exists := detail["details"]; !exists || value != nil {
					t.Fatal("details 必须为 null")
				}
				if response.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("拒绝响应可能缓存")
				}
			}
			var record map[string]any
			if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			level := "INFO"
			if response.Code >= 400 {
				level = "WARN"
			}
			if response.Code >= 500 {
				level = "ERROR"
			}
			if record["request_id"] != requestID || record["status"] != float64(response.Code) || record["level"] != level {
				t.Fatal("响应和完成日志未关联")
			}
			if strings.Contains(logs.String(), "private-") || strings.Contains(response.Body.String(), "private-") {
				t.Fatal("响应或日志泄漏敏感输入")
			}
		})
	}
}

func TestCORSOverRealHTTP(t *testing.T) {
	// 1. 真实 TCP 验证预检无正文、拒绝的 HEAD 无正文，以及合法实际请求保持原 JSON。
	origins := []string{"http://127.0.0.1:5173"}
	router := NewRouter(logging.New(io.Discard), origins, func(context.Context) bool { return true })
	origins[0] = "https://private-mutated.example"
	server := httptest.NewServer(router)
	defer server.Close()
	for _, testCase := range []struct {
		method, origin, preflight string
		status                    int
		body                      string
	}{
		{method: "OPTIONS", origin: "http://127.0.0.1:5173", preflight: "POST", status: 204},
		{method: "GET", origin: "http://127.0.0.1:5173", status: 200, body: `{"status":"ok"}`},
		{method: "HEAD", origin: "https://private-mutated.example", status: 403},
	} {
		request, err := http.NewRequest(testCase.method, server.URL+"/health/live", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Origin", testCase.origin)
		if testCase.preflight != "" {
			request.Header.Set("Access-Control-Request-Method", testCase.preflight)
		}
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, readError := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if readError != nil || response.StatusCode != testCase.status || string(body) != testCase.body {
			t.Fatalf("真实 HTTP status=%d body=%s err=%v", response.StatusCode, body, readError)
		}
		if _, err := uuid.Parse(response.Header.Get("X-Request-ID")); err != nil {
			t.Fatal("真实响应没有请求 ID")
		}
	}
}

func TestCORSConcurrentOrigins(t *testing.T) {
	// 1. 让允许、拒绝、无来源与预检请求并发共用同一个路由，检查来源头不串请求。
	var checks atomic.Int64
	router := NewRouter(logging.New(io.Discard), []string{"https://one.example", "https://two.example"}, func(context.Context) bool { checks.Add(1); return true })
	type outcome struct {
		status           int
		origin, expected string
		want             int
	}
	outcomes := make(chan outcome, 32)
	for requestIndex := range 32 {
		go func(requestIndex int) {
			origin, method, expected, want := "https://one.example", "GET", "https://one.example", 200
			switch requestIndex % 5 {
			case 1:
				origin, expected = "https://two.example", "https://two.example"
			case 2:
				origin, expected, want = "https://private-unknown.example", "", 403
			case 3:
				origin, expected = "", ""
			case 4:
				method, want = "OPTIONS", 204
			}
			request := httptest.NewRequest(method, "/health/ready", nil)
			if origin != "" {
				request.Header.Set("Origin", origin)
			}
			if method == "OPTIONS" {
				request.Header.Set("Access-Control-Request-Method", "GET")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			outcomes <- outcome{status: response.Code, origin: response.Header().Get("Access-Control-Allow-Origin"), expected: expected, want: want}
		}(requestIndex)
	}
	for range 32 {
		result := <-outcomes
		if result.status != result.want || result.origin != result.expected {
			t.Fatalf("并发响应串线: %+v", result)
		}
	}
	if checks.Load() != 20 {
		t.Fatalf("拒绝或预检执行了依赖检查: %d", checks.Load())
	}
}
