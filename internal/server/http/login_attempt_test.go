package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/security"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

// newLoginAttemptTestRouter 只注册测试端点；非限流的凭证状态在此使用测试映射。
func newLoginAttemptTestRouter(logger *slog.Logger, authenticate func(context.Context, string, string) (*domain.VerifiedAccount, error)) *gin.Engine {
	allowedOrigins := []string{"http://127.0.0.1:5173"}
	policy := security.NewLoginSourcePolicy(allowedOrigins)
	limiter := service.NewLoginFailureLimiter()
	gate := security.NewLoginAttemptGate(limiter.Begin, authenticate)
	router := NewRouter(logger, allowedOrigins, func(context.Context) bool { return true })
	router.POST("/test-login-attempt", func(requestContext *gin.Context) {
		// 1. 读取 DTO 并校验来源，非法输入不能消耗限流名额。
		var input dto.LoginRequest
		if !binding.ReadJSON(requestContext, &input) {
			return
		}
		if validationError := input.Validate(); validationError != nil {
			handler.InvalidArgument(requestContext, validationError)
			return
		}
		if !policy.Allows(requestContext.Request, input.ClientType) {
			handler.ClientSourceForbidden(requestContext)
			return
		}
		// 2. 适配器处理名额与结算；受限使用本轮统一 429。
		_, retryAfter, err := gate.Authenticate(requestContext.Request, input.Username, input.Password)
		if retryAfter > 0 {
			handler.RateLimited(requestContext, retryAfter)
			return
		}
		// 这里只模拟尚未实现的凭证 HTTP 映射，不声明生产登录已可用。
		if errors.Is(err, domain.ErrInvalidCredentials) {
			requestContext.AbortWithStatus(401)
			return
		}
		if err != nil {
			handler.InternalError(requestContext, nil)
			return
		}
		requestContext.Status(204)
	})
	return router
}

func loginAttemptTestRequest(username string) *http.Request {
	input := dto.LoginRequest{Username: username, Password: "private-password", InstallationID: "28ee758a-0727-42de-ae7d-3bb33155e148", DeviceName: "private-device", Platform: "WEB", ClientType: "web"}
	encoded, _ := json.Marshal(input)
	request := httptest.NewRequest("POST", "/test-login-attempt?token=private-query", bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://127.0.0.1:5173")
	request.Header.Set(security.ClientHeader, "web")
	request.Header.Set("X-Request-ID", "28EE758A-0727-42DE-AE7D-3BB33155E148")
	return request
}

func TestRateLimitedResponseAndRounding(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		wait    time.Duration
		seconds string
	}{
		{name: "negative", wait: -time.Second, seconds: "1"},
		{name: "zero", seconds: "1"},
		{name: "one nanosecond", wait: time.Nanosecond, seconds: "1"},
		{name: "one second", wait: time.Second, seconds: "1"},
		{name: "fraction rounds up", wait: time.Second + time.Nanosecond, seconds: "2"},
		{name: "minute fraction", wait: 59*time.Second + time.Nanosecond, seconds: "60"},
		{name: "minute", wait: time.Minute, seconds: "60"},
		{name: "maximum duration", wait: time.Duration(1<<63 - 1), seconds: "9223372037"},
		{name: "minimum duration", wait: time.Duration(-1 << 63), seconds: "1"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var logs bytes.Buffer
			router := NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
			afterCalled := false
			router.GET("/test-rate", func(requestContext *gin.Context) { handler.RateLimited(requestContext, testCase.wait) }, func(*gin.Context) { afterCalled = true })
			request := httptest.NewRequest("GET", "/test-rate?token=private-query", nil)
			request.Header.Set("Origin", "http://127.0.0.1:5173")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != 429 || afterCalled || response.Header().Get("Retry-After") != testCase.seconds || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Access-Control-Expose-Headers") != "X-Request-ID, Retry-After" {
				t.Fatalf("429 头/终止规则错误 status=%d headers=%v", response.Code, response.Header())
			}
			var body map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			detail, valid := body["error"].(map[string]any)
			if !valid || len(body) != 2 || len(detail) != 3 || detail["code"] != "RATE_LIMITED" || detail["message"] != "登录尝试过于频繁，请稍后重试" || body["request_id"] != response.Header().Get("X-Request-ID") {
				t.Fatal("429 JSON 结构错误")
			}
			if value, exists := detail["details"]; !exists || value != nil {
				t.Fatal("details 必须明确为 null")
			}
			var record map[string]any
			if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			if record["request_id"] != body["request_id"] || record["level"] != "WARN" || record["status"] != float64(429) || strings.Contains(logs.String(), "private-") {
				t.Fatal("429 日志错误或泄漏输入")
			}
		})
	}
}

func TestLoginAttemptHTTPThresholdAndForwarding(t *testing.T) {
	for _, dimension := range []string{"ip", "username"} {
		t.Run(dimension, func(t *testing.T) {
			var logs bytes.Buffer
			calls := 0
			router := newLoginAttemptTestRouter(logging.New(&logs), func(context.Context, string, string) (*domain.VerifiedAccount, error) {
				calls++
				return nil, domain.ErrInvalidCredentials
			})
			for attemptIndex := range 12 {
				username := "root"
				if dimension == "ip" {
					username = fmt.Sprintf("user_%d", attemptIndex)
				}
				request := loginAttemptTestRequest(username)
				request.RemoteAddr = fmt.Sprintf("192.0.2.1:%d", 12000+attemptIndex)
				if dimension == "username" {
					request.RemoteAddr = fmt.Sprintf("192.0.2.%d:1234", attemptIndex+1)
				}
				request.Header.Set("X-Forwarded-For", fmt.Sprintf("198.51.100.%d", attemptIndex+1))
				request.Header.Set("X-Real-IP", "203.0.113.1")
				request.Header.Set("Forwarded", "for=203.0.113.2")
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				want := 401
				if attemptIndex >= 10 {
					want = 429
				}
				if response.Code != want {
					t.Fatalf("第 %d 次状态=%d", attemptIndex+1, response.Code)
				}
				if want == 429 {
					seconds, err := strconv.Atoi(response.Header().Get("Retry-After"))
					if err != nil || seconds < 1 || seconds > 60 || !strings.Contains(response.Body.String(), `"code":"RATE_LIMITED"`) {
						t.Fatal("受限响应缺少有效等待建议")
					}
				}
			}
			if calls != 10 || strings.Contains(logs.String(), "private-") {
				t.Fatal("限流后仍执行凭证查询或泄漏请求内容")
			}
		})
	}
}

func TestLoginAttemptHTTPSourceAndPanicRelease(t *testing.T) {
	// 来源/预检不触及 Gate；异常路径不能漏掉已预留的名额。
	var logs bytes.Buffer
	calls := 0
	panicMode := true
	router := newLoginAttemptTestRouter(logging.New(&logs), func(context.Context, string, string) (*domain.VerifiedAccount, error) {
		calls++
		if panicMode {
			panic("private-credential-panic")
		}
		return nil, domain.ErrInvalidCredentials
	})
	for range 12 {
		request := loginAttemptTestRequest("root")
		request.Header.Del(security.ClientHeader)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != 403 {
			t.Fatal("来源校验未先行")
		}
	}
	preflight := httptest.NewRequest("OPTIONS", "/test-login-attempt", nil)
	preflight.Header.Set("Origin", "http://127.0.0.1:5173")
	preflight.Header.Set("Access-Control-Request-Method", "POST")
	preflight.Header.Set("Access-Control-Request-Headers", "content-type,x-cloudnest-client")
	preflightResponse := httptest.NewRecorder()
	router.ServeHTTP(preflightResponse, preflight)
	if calls != 0 || preflightResponse.Code != 204 {
		t.Fatal("来源拒绝或预检进入了凭证步骤")
	}
	for range 12 {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, loginAttemptTestRequest("root"))
		if response.Code != 500 {
			t.Fatal("异常路径泄漏预留，后续请求意外被限流")
		}
	}
	panicMode = false
	for attemptIndex := range 11 {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, loginAttemptTestRequest("root"))
		want := 401
		if attemptIndex == 10 {
			want = 429
		}
		if response.Code != want {
			t.Fatal("异常消耗了失败次数或未正常恢复计数")
		}
	}
	if strings.Contains(logs.String(), "private-") {
		t.Fatal("异常日志回显了请求或 panic 载荷")
	}
}

func TestLoginAttemptOverRealHTTPConcurrent(t *testing.T) {
	// 持住 10 个已放行请求，让剩余 22 个真实 TCP 请求立即得到 429。
	started := make(chan struct{}, 32)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	var calls atomic.Int64
	router := newLoginAttemptTestRouter(logging.New(io.Discard), func(context.Context, string, string) (*domain.VerifiedAccount, error) {
		calls.Add(1)
		started <- struct{}{}
		<-release
		return nil, domain.ErrInvalidCredentials
	})
	server := httptest.NewServer(router)
	defer server.Close()
	defer releaseAll()
	client := server.Client()
	client.Timeout = 5 * time.Second
	type outcome struct {
		status int
		retry  string
		err    error
	}
	outcomes := make(chan outcome, 32)
	for range 32 {
		go func() {
			request := loginAttemptTestRequest("root")
			request.URL.Scheme, request.URL.Host, request.RequestURI = "http", strings.TrimPrefix(server.URL, "http://"), ""
			response, err := client.Do(request)
			if err != nil {
				outcomes <- outcome{err: err}
				return
			}
			_, readError := io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			outcomes <- outcome{status: response.StatusCode, retry: response.Header.Get("Retry-After"), err: readError}
		}()
	}
	for range 10 {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("未放行预期的十个在途请求")
		}
	}
	for range 22 {
		select {
		case result := <-outcomes:
			if result.err != nil || result.status != 429 || result.retry == "" {
				t.Fatalf("在途限制失效: %+v", result)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("受限请求没有及时返回")
		}
	}
	releaseAll()
	for range 10 {
		result := <-outcomes
		if result.err != nil || result.status != 401 {
			t.Fatalf("已放行请求结果错误: %+v", result)
		}
	}
	if calls.Load() != 10 {
		t.Fatal("并发限流后仍执行了额外凭证计算")
	}
}
