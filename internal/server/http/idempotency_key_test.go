package http

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
)

func TestIdempotencyKeyHTTPFlow(t *testing.T) {
	for _, testCase := range []struct {
		name, body, key, code, field         string
		values                               []string
		status, parseCalls, jsonCalls        int
		noAuthorization, badToken, preflight bool
	}{
		{name: "uuid", values: []string{"28ee758a-0727-42de-ae7d-3bb33155e148"}, key: "28ee758a-0727-42de-ae7d-3bb33155e148", status: 204, parseCalls: 1, jsonCalls: 1},
		{name: "opaque key case preserved", values: []string{"Request-007"}, key: "Request-007", status: 204, parseCalls: 1, jsonCalls: 1},
		{name: "http whitespace", values: []string{" \tRequest-007\t "}, key: "Request-007", status: 204, parseCalls: 1, jsonCalls: 1},
		{name: "maximum key", values: []string{strings.Repeat("A", 128)}, key: strings.Repeat("A", 128), status: 204, parseCalls: 1, jsonCalls: 1},
		{name: "missing before body", body: `{"private-broken":`, status: 400, field: "Idempotency-Key", parseCalls: 1},
		{name: "empty", values: []string{""}, status: 400, field: "Idempotency-Key", parseCalls: 1},
		{name: "blank", values: []string{" \t"}, status: 400, field: "Idempotency-Key", parseCalls: 1},
		{name: "duplicate identical", values: []string{"same", "same"}, status: 400, field: "Idempotency-Key", parseCalls: 1},
		{name: "duplicate distinct", values: []string{"first", "second"}, status: 400, field: "Idempotency-Key", parseCalls: 1},
		{name: "comma merged", values: []string{"private-first,private-second"}, status: 400, field: "Idempotency-Key", parseCalls: 1},
		{name: "long key", values: []string{strings.Repeat("a", 129)}, status: 400, field: "Idempotency-Key", parseCalls: 1},
		{name: "internal space", values: []string{"private first"}, status: 400, field: "Idempotency-Key", parseCalls: 1},
		{name: "internal tab", values: []string{"private\tfirst"}, status: 400, field: "Idempotency-Key", parseCalls: 1},
		{name: "unicode", values: []string{"请求编号"}, status: 400, field: "Idempotency-Key", parseCalls: 1},
		{name: "control in direct request", values: []string{"private-key\x00"}, status: 400, field: "Idempotency-Key", parseCalls: 1},
		{name: "valid key before invalid json", values: []string{"Request-007"}, key: "Request-007", body: `{"private-broken":`, status: 400, parseCalls: 1, jsonCalls: 1},
		{name: "body cannot provide header", body: `{"parent_id":"11","name":"资料","idempotency_key":"private-body"}`, status: 400, field: "Idempotency-Key", parseCalls: 1},
		{name: "extra body key rejected", values: []string{"Request-007"}, key: "Request-007", body: `{"parent_id":"11","name":"资料","idempotency_key":"private-body"}`, status: 400, parseCalls: 1, jsonCalls: 1},
		{name: "body field failure follows header", values: []string{"Request-007"}, key: "Request-007", body: `{"parent_id":"0","name":"资料"}`, status: 400, field: "parent_id", parseCalls: 1, jsonCalls: 1},
		{name: "missing bearer first", noAuthorization: true, body: `{"private-broken":`, status: 401, code: "AUTH_REQUIRED"},
		{name: "rejected bearer first", badToken: true, body: `{"private-broken":`, status: 401, code: "AUTH_REQUIRED"},
		{name: "cors preflight", preflight: true, noAuthorization: true, status: 204},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 测试入口依次组合认证、幂等头、JSON 读取和创建请求 DTO。
			var logs bytes.Buffer
			parsedKey := ""
			var accepted *dto.CreateDirectoryInput
			parseCalls, jsonCalls, downstreamCalls := 0, 0, 0
			router := NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
			protected := middleware.RequireAuthorization(func(_ context.Context, token string) (*domain.Actor, error) {
				if token != "fixture-access" {
					return nil, domain.ErrInvalidAccessToken
				}
				return &domain.Actor{UserID: 7, DeviceID: 13, SessionID: 17}, nil
			}, handler.AuthorizationFailure)
			router.POST("/test-idempotency-key", protected, func(requestContext *gin.Context) {
				parseCalls++
				key, fieldError := dto.ParseIdempotencyKey(requestContext.Request)
				if fieldError != nil {
					handler.InvalidArgument(requestContext, fieldError)
					return
				}
				parsedKey = key
				jsonCalls++
				var request dto.CreateDirectoryRequest
				if !binding.ReadJSON(requestContext, &request) {
					return
				}
				input, fieldError := dto.ParseCreateDirectoryRequest(request)
				if fieldError != nil {
					handler.InvalidArgument(requestContext, fieldError)
					return
				}
				accepted = input
				requestContext.AbortWithStatus(204)
			}, func(*gin.Context) { downstreamCalls++ })
			body := testCase.body
			if body == "" {
				body = `{"parent_id":"11","name":"资料"}`
			}
			method := "POST"
			if testCase.preflight {
				method = "OPTIONS"
			}
			request := httptest.NewRequest(method, "/test-idempotency-key?Idempotency-Key=private-query", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Cookie", "Idempotency-Key=private-cookie")
			request.Header.Set("Origin", "http://127.0.0.1:5173")
			request.Header.Set("X-Request-ID", "28EE758A-0727-42DE-AE7D-3BB33155E148")
			if !testCase.noAuthorization {
				token := "fixture-access"
				if testCase.badToken {
					token = "private-bad-access"
				}
				request.Header.Set("Authorization", "Bearer "+token)
			}
			for _, value := range testCase.values {
				request.Header.Add("Idempotency-Key", value)
			}
			if testCase.preflight {
				request.Header.Set("Access-Control-Request-Method", "POST")
				request.Header.Set("Access-Control-Request-Headers", "authorization,content-type,idempotency-key")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			// 2. 核对提前失败顺序、头值、字段错误及日志保护。
			if response.Code != testCase.status || parseCalls != testCase.parseCalls || jsonCalls != testCase.jsonCalls || parsedKey != testCase.key || downstreamCalls != 0 {
				t.Fatal("执行顺序、解析结果或响应状态错误", response.Code, parseCalls, jsonCalls)
			}
			if response.Header().Get("X-Request-ID") != "28ee758a-0727-42de-ae7d-3bb33155e148" || response.Header().Get("Access-Control-Allow-Origin") != "http://127.0.0.1:5173" {
				t.Fatal("请求 ID 或 CORS 头错误")
			}
			if testCase.status == 204 {
				if response.Body.Len() != 0 || (!testCase.preflight && (accepted == nil || accepted.ParentID != 11 || accepted.Name != "资料")) {
					t.Fatal("成功输入或空响应错误")
				}
				if testCase.preflight && (!strings.Contains(response.Header().Get("Access-Control-Allow-Headers"), "Idempotency-Key") || accepted != nil) {
					t.Fatal("预检未允许幂等头或进入了请求解析")
				}
			} else {
				code := testCase.code
				if code == "" {
					code = "INVALID_ARGUMENT"
				}
				var body dto.ErrorResponse
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Error.Code != code || body.RequestID != response.Header().Get("X-Request-ID") || accepted != nil || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("统一失败响应错误", err)
				}
				if testCase.field == "" {
					if body.Error.Details != nil {
						t.Fatal("绑定或认证失败应保留空 details")
					}
				} else if len(body.Error.Details) != 1 || body.Error.Details["field"] != testCase.field {
					t.Fatal("字段错误定位错误")
				}
			}
			if strings.Contains(logs.String(), "private-") || strings.Contains(response.Body.String(), "private-") || strings.Contains(logs.String(), "Request-007") {
				t.Fatal("日志或错误响应回显了键、正文、查询参数或凭证")
			}
		})
	}
}

func TestIdempotencyKeyHTTPTransport(t *testing.T) {
	// 1. 使用真实 HTTP 服务核对头名称大小写、OWS 和重复字段的实际传输。
	var logs bytes.Buffer
	observed := make(chan string, 1)
	router := NewRouter(logging.New(&logs), nil, func(context.Context) bool { return true })
	router.POST("/test-idempotency-wire", func(requestContext *gin.Context) {
		key, fieldError := dto.ParseIdempotencyKey(requestContext.Request)
		if fieldError != nil {
			handler.InvalidArgument(requestContext, fieldError)
			return
		}
		observed <- key
		requestContext.AbortWithStatus(204)
	})
	server := httptest.NewServer(router)
	defer server.Close()
	client := server.Client()
	client.Timeout = 3 * time.Second
	for _, testCase := range []struct {
		name, value, want string
		status            int
		duplicateCase     bool
	}{
		{name: "lowercase field name", value: "Request-007", want: "Request-007", status: 204},
		{name: "http whitespace", value: " \tRequest-007\t ", want: "Request-007", status: 204},
		{name: "maximum key", value: strings.Repeat("A", 128), want: strings.Repeat("A", 128), status: 204},
		{name: "opaque punctuation", value: "azAZ09-._~+/==", want: "azAZ09-._~+/==", status: 204},
		{name: "merged values", value: "private-first,private-second", status: 400},
		{name: "internal whitespace", value: "private first", status: 400},
		{name: "overflow", value: strings.Repeat("a", 129), status: 400},
		{name: "different case duplicates", value: "private-first", duplicateCase: true, status: 400},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			request, err := http.NewRequest("POST", server.URL+"/test-idempotency-wire", strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			// 直接设置不同大小写的 map 键，让客户端实际发出不同拼写的字段行。
			request.Header["idempotency-key"] = []string{testCase.value}
			if testCase.duplicateCase {
				request.Header["IDEMPOTENCY-KEY"] = []string{"private-second"}
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			content, readError := io.ReadAll(response.Body)
			closeError := response.Body.Close()
			if readError != nil || closeError != nil || response.StatusCode != testCase.status {
				t.Fatal("实际 HTTP 传输状态错误", readError, closeError, response.StatusCode)
			}
			if testCase.status == 204 {
				select {
				case key := <-observed:
					if key != testCase.want || len(content) != 0 {
						t.Fatal("实际传输改变了键原文")
					}
				default:
					t.Fatal("实际服务未接收有效键")
				}
			} else {
				var body dto.ErrorResponse
				if err := json.Unmarshal(content, &body); err != nil || body.Error.Code != "INVALID_ARGUMENT" || body.Error.Details["field"] != "Idempotency-Key" || len(observed) != 0 || bytes.Contains(content, []byte("private-")) {
					t.Fatal("实际传输字段错误或重复值处理错误", err)
				}
			}
		})
	}
	server.Close()
	if strings.Contains(logs.String(), "private-") || strings.Contains(logs.String(), "Request-007") {
		t.Fatal("实际服务日志回显了幂等键")
	}
}
