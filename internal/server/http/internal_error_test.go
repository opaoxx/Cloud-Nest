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
	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
)

func TestRecoveredPanicJSONAndPrivacy(t *testing.T) {
	// 1. 使用不同异常载荷及待发送的响应头，均只能得到固定的公开错误信息。
	testCases := []struct {
		name           string
		payload        any
		pendingHeaders bool
		requestID      string
	}{
		{name: "string", payload: "private-panic-string"},
		{name: "error", payload: errors.New("private-panic-error"), requestID: "28EE758A-0727-42DE-AE7D-3BB33155E148"},
		{name: "structured payload", payload: map[string]string{"secret": "private-panic-map"}},
		{name: "number", payload: 42},
		{name: "pending response headers", payload: "private-panic-headers", pendingHeaders: true},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			var output bytes.Buffer
			previousErrorWriter := gin.DefaultErrorWriter
			gin.DefaultErrorWriter = &output
			t.Cleanup(func() { gin.DefaultErrorWriter = previousErrorWriter })
			router := NewRouter(logging.New(&output), func(ctx context.Context) bool { return true })
			router.GET("/test-error", func(requestContext *gin.Context) {
				if testCase.pendingHeaders {
					requestContext.Status(http.StatusCreated)
					requestContext.Header("Content-Length", "3")
					requestContext.Header("Content-Encoding", "gzip")
					requestContext.Header("Content-Type", "application/octet-stream")
				}
				panic(testCase.payload)
			})
			request := httptest.NewRequest(http.MethodGet, "/test-error?token=private-query", strings.NewReader("private-body"))
			request.Header.Set("Cookie", "session=private-cookie")
			if testCase.requestID != "" {
				request.Header.Set("X-Request-ID", testCase.requestID)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			// 2. 未提交的响应统一为 500；长度、编码和类型不受原 Handler 的待发送头影响。
			if response.Code != 500 || response.Header().Get("Content-Type") != "application/json; charset=utf-8" || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Content-Length") != "" || response.Header().Get("Content-Encoding") != "" {
				t.Fatal("500 响应状态或头不符合约定")
			}
			var body map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			detail, valid := body["error"].(map[string]any)
			if !valid || len(body) != 2 || len(detail) != 3 || detail["code"] != "INTERNAL_ERROR" || detail["message"] != "服务内部错误，请稍后重试" {
				t.Fatal("500 错误结构不符合契约")
			}
			if details, exists := detail["details"]; !exists || details != nil {
				t.Fatal("details 必须明确为 null")
			}
			requestID, _ := body["request_id"].(string)
			parsedID, err := uuid.Parse(requestID)
			if err != nil || parsedID.Version() != 4 || requestID != response.Header().Get("X-Request-ID") {
				t.Fatal("500 正文与响应头没有使用同一个 UUID v4")
			}
			if testCase.requestID != "" && requestID != strings.ToLower(testCase.requestID) {
				t.Fatal("合法请求 ID 未被保留")
			}

			// 3. 唯一完成日志为 ERROR，只有布尔异常标记，没有原始异常或请求数据。
			var record map[string]any
			if err := json.Unmarshal(output.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			if record["status"] != float64(500) || record["level"] != "ERROR" || record["request_id"] != requestID || record["recovered_panic"] != true || len(record) != 10 {
				t.Fatal("异常日志的状态、ID 或标记错误")
			}
			if strings.Contains(response.Body.String(), "private-") || strings.Contains(output.String(), "private-") {
				t.Fatal("响应或日志泄漏了敏感标记")
			}
		})
	}
}

func TestRecoveredPanicPreservesCommittedResponse(t *testing.T) {
	// 1. 已发送头或正文时不能回滚 HTTP，不能在原正文后拼接错误 JSON。
	for _, headersOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "body committed", true: "headers committed"}[headersOnly], func(t *testing.T) {
			var output bytes.Buffer
			wantStatus := http.StatusOK
			if headersOnly {
				wantStatus = http.StatusAccepted
			}
			router := NewRouter(logging.New(&output), func(ctx context.Context) bool { return true })
			router.GET("/test-partial", func(requestContext *gin.Context) {
				requestContext.Header("Content-Type", "text/plain")
				requestContext.Header("Cache-Control", "max-age=60")
				requestContext.Status(wantStatus)
				if headersOnly {
					requestContext.Writer.WriteHeaderNow()
				} else {
					_, _ = requestContext.Writer.WriteString("already-written")
				}
				panic("private-committed-panic")
			})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/test-partial", nil))
			wantBody := "already-written"
			if headersOnly {
				wantBody = ""
			}
			if response.Code != wantStatus || response.Body.String() != wantBody || response.Header().Get("Content-Type") != "text/plain" || response.Header().Get("Cache-Control") != "max-age=60" {
				t.Fatal("已提交的响应被覆盖或拼接")
			}

			// 2. 日志保留实际状态，但必须识别异常，不能误记为普通成功。
			var record map[string]any
			if err := json.Unmarshal(output.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			if record["status"] != float64(wantStatus) || record["level"] != "ERROR" || record["recovered_panic"] != true || record["request_id"] != response.Header().Get("X-Request-ID") || strings.Contains(output.String(), "private-") {
				t.Fatal("已提交响应的异常没有被正确、脱敏地记录")
			}
		})
	}
}

func TestRecoveredPanicOverRealHTTP(t *testing.T) {
	// 1. 用生产路由的就绪回调注入异常，真实 TCP 服务不需要业务数据库。
	var output bytes.Buffer
	router := NewRouter(logging.New(&output), func(ctx context.Context) bool {
		panic("private-wire-panic")
	})
	router.HEAD("/test-error", func(requestContext *gin.Context) { panic("private-head-panic") })
	router.GET("/test-pending", func(requestContext *gin.Context) {
		requestContext.Header("Content-Length", "3")
		requestContext.Header("Content-Encoding", "gzip")
		requestContext.Header("Content-Type", "application/octet-stream")
		panic("private-pending-panic")
	})
	server := httptest.NewServer(router)
	defer server.Close()
	client := server.Client()
	for _, path := range []string{"/health/ready", "/test-pending"} {
		response, err := client.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
			_ = response.Body.Close()
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != 500 || body["request_id"] != response.Header.Get("X-Request-ID") || response.Header.Get("Content-Encoding") != "" {
			t.Fatal("真实 HTTP 没有返回统一 500 与关联 ID，或遗留了错误编码")
		}
	}

	// 2. 同一进程发生异常后仍能响应正常请求，HEAD 不发送错误正文。
	response, err := client.Get(server.URL + "/health/live")
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != 200 || string(content) != `{"status":"ok"}` {
		t.Fatal("异常后服务无法继续响应正常请求")
	}
	response, err = client.Head(server.URL + "/test-error")
	if err != nil {
		t.Fatal(err)
	}
	content, err = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != 500 || len(content) != 0 || response.Header.Get("X-Request-ID") == "" {
		t.Fatal("真实 HEAD 响应未遵守状态、标识或无正文约定")
	}
}
