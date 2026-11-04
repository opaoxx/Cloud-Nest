package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
)

func TestRequestLogStatusAndPrivacy(t *testing.T) {
	// 1. 覆盖最终状态、提前终止与异常恢复，输入中放入仅供测试的敏感标记。
	testCases := []struct {
		name      string
		status    int
		level     string
		abort     bool
		panic     bool
		defaultOK bool
		canceled  bool
	}{
		{name: "success", status: 200, level: "INFO"},
		{name: "no content", status: 204, level: "INFO"},
		{name: "redirect", status: 302, level: "INFO"},
		{name: "not found", status: 404, level: "WARN"},
		{name: "abort", status: 401, level: "WARN", abort: true},
		{name: "dependency failure", status: 503, level: "ERROR"},
		{name: "recovered panic", status: 500, level: "ERROR", panic: true},
		{name: "default status", status: 200, level: "INFO", defaultOK: true},
		{name: "canceled request", status: 503, level: "ERROR", canceled: true},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			var output bytes.Buffer
			// 同时捕获默认异常输出，防止以后误用默认 Recovery 而重新泄漏数据。
			previousErrorWriter := gin.DefaultErrorWriter
			gin.DefaultErrorWriter = &output
			t.Cleanup(func() { gin.DefaultErrorWriter = previousErrorWriter })
			router := NewRouter(logging.New(&output), func(ctx context.Context) bool { return false })
			router.GET("/test", func(requestContext *gin.Context) {
				_, _ = io.ReadAll(requestContext.Request.Body)
				_ = requestContext.Error(errors.New("private-context-error"))
				if testCase.panic {
					panic("private-panic-payload")
				}
				if testCase.abort {
					requestContext.AbortWithStatus(testCase.status)
					return
				}
				if !testCase.defaultOK {
					requestContext.JSON(testCase.status, gin.H{"value": "private-response-body"})
				}
			})
			request := httptest.NewRequest(http.MethodGet, "/test?token=private-query", strings.NewReader("private-request-body"))
			request.Header.Set("X-Request-ID", "28EE758A-0727-42DE-AE7D-3BB33155E148")
			request.Header.Set("Authorization", "Bearer private-auth")
			request.Header.Set("Cookie", "session=private-cookie")
			request.Header.Set("X-Custom-Secret", "private-header")
			if testCase.canceled {
				ctx, cancel := context.WithCancel(request.Context())
				cancel()
				request = request.WithContext(ctx)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			// 2. 一次请求只能有一条完成日志，且包含最终状态和相同的请求 ID。
			decoder := json.NewDecoder(&output)
			var record map[string]any
			if err := decoder.Decode(&record); err != nil {
				t.Fatalf("日志不是单条 JSON: %v", err)
			}
			var extra any
			if err := decoder.Decode(&extra); err != io.EOF {
				t.Fatal("同一请求输出了重复日志或额外转储")
			}
			if response.Code != testCase.status || record["status"] != float64(testCase.status) || record["level"] != testCase.level || record["request_id"] != response.Header().Get("X-Request-ID") || record["request_id"] != "28ee758a-0727-42de-ae7d-3bb33155e148" {
				t.Fatal("日志与最终响应的状态、级别或 ID 不一致")
			}
			if record["method"] != "GET" || record["path"] != "/test" || record["msg"] != "HTTP 请求完成" || record["service"] != "cloudnest-api" || record["time"] == nil || len(record) != 9 {
				t.Fatal("日志字段不符合约定")
			}
			if duration, ok := record["duration_ms"].(float64); !ok || duration < 0 {
				t.Fatal("耗时必须为非负的毫秒数字")
			}

			// 3. 日志只含允许的元数据，不收集原始请求、响应或异常内容。
			encoded, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "private-") {
				t.Fatal("日志泄漏了查询、头、正文、错误或 panic 载荷")
			}
		})
	}
}

func TestRequestLogConcurrentCorrelation(t *testing.T) {
	// 1. 多个并发请求共用同一个日志对象，检查每个响应与日志的一一对应。
	var output bytes.Buffer
	router := NewRouter(logging.New(&output), func(ctx context.Context) bool { return true })
	const requestCount = 32
	responses := make(chan *httptest.ResponseRecorder, requestCount)
	for range requestCount {
		go func() {
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health/live", nil))
			responses <- response
		}()
	}
	responseIDs := make(map[string]bool)
	for range requestCount {
		response := <-responses
		requestID := response.Header().Get("X-Request-ID")
		if response.Code != 200 || requestID == "" || responseIDs[requestID] {
			t.Fatal("并发响应缺少唯一请求 ID")
		}
		responseIDs[requestID] = true
	}

	// 2. 所有请求完成后读取日志；同一个 ID 只能出现一次，不能串到其他请求。
	decoder := json.NewDecoder(&output)
	for range requestCount {
		var record map[string]any
		if err := decoder.Decode(&record); err != nil {
			t.Fatal(err)
		}
		requestID, _ := record["request_id"].(string)
		if !responseIDs[requestID] || record["status"] != float64(200) || record["path"] != "/health/live" {
			t.Fatal("并发日志重复、串号或结果错误")
		}
		delete(responseIDs, requestID)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF || len(responseIDs) != 0 {
		t.Fatal("日志数量与请求数量不一致")
	}
}
