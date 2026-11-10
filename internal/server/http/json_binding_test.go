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
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
)

// jsonBindingInput 只用于验证解析规则，不是生产登录 DTO。
type jsonBindingInput struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func TestReadJSONInputPolicy(t *testing.T) {
	// 1. 把读取器放进生产中间件链，确认只有完整成功时才执行后续逻辑。
	testCases := []struct {
		name         string
		body         string
		contentTypes []string
		encodings    []string
		wantStatus   int
		wantCode     string
		wantName     string
		wantCount    int
	}{
		{name: "object", body: `{"name":"学习笔记","count":2}`, wantStatus: 204, wantName: "学习笔记", wantCount: 2},
		{name: "whitespace", body: " \r\n\t{\"name\":\"notes\"}\r\n\t ", wantStatus: 204, wantName: "notes"},
		{name: "utf8 charset", body: `{}`, contentTypes: []string{"Application/JSON; Charset=\"UTF-8\""}, wantStatus: 204},
		{name: "identity encoding", body: `{}`, encodings: []string{"identity"}, wantStatus: 204},
		{name: "additional media parameter", body: `{}`, contentTypes: []string{"application/json; profile=test"}, wantStatus: 204},
		{name: "missing fields deferred to validator", body: `{}`, wantStatus: 204},
		{name: "null field deferred to validator", body: `{"name":null}`, wantStatus: 204},
		{name: "empty body", wantStatus: 400, wantCode: "INVALID_ARGUMENT"},
		{name: "only whitespace", body: " \r\n\t", wantStatus: 400, wantCode: "INVALID_ARGUMENT"},
		{name: "root null", body: `null`, wantStatus: 400, wantCode: "INVALID_ARGUMENT"},
		{name: "root array", body: `[]`, wantStatus: 400, wantCode: "INVALID_ARGUMENT"},
		{name: "root string", body: `"private-string"`, wantStatus: 400, wantCode: "INVALID_ARGUMENT"},
		{name: "root number", body: `42`, wantStatus: 400, wantCode: "INVALID_ARGUMENT"},
		{name: "root boolean", body: `true`, wantStatus: 400, wantCode: "INVALID_ARGUMENT"},
		{name: "truncated object", body: `{"name":"private-name"`, wantStatus: 400, wantCode: "INVALID_ARGUMENT"},
		{name: "trailing comma", body: `{"name":"private-name",}`, wantStatus: 400, wantCode: "INVALID_ARGUMENT"},
		{name: "wrong field type", body: `{"name":42}`, wantStatus: 400, wantCode: "INVALID_ARGUMENT"},
		{name: "wrong integer type", body: `{"count":1.5}`, wantStatus: 400, wantCode: "INVALID_ARGUMENT"},
		{name: "integer overflow", body: `{"count":9999999999999999999999999999999}`, wantStatus: 400, wantCode: "INVALID_ARGUMENT"},
		{name: "unknown field after partial decode", body: `{"name":"private-partial","private-unknown":"private-value"}`, wantStatus: 400, wantCode: "INVALID_ARGUMENT"},
		{name: "second object", body: `{} {"name":"private-second"}`, wantStatus: 400, wantCode: "INVALID_ARGUMENT"},
		{name: "second null", body: `{} null`, wantStatus: 400, wantCode: "INVALID_ARGUMENT"},
		{name: "trailing garbage", body: `{} private-garbage`, wantStatus: 400, wantCode: "INVALID_ARGUMENT"},
		{name: "invalid utf8", body: "{\"name\":\"\xff\"}", wantStatus: 400, wantCode: "INVALID_ARGUMENT"},
		{name: "utf8 bom", body: "\xef\xbb\xbf{}", wantStatus: 400, wantCode: "INVALID_ARGUMENT"},
		{name: "missing content type", body: `{}`, contentTypes: []string{}, wantStatus: 415, wantCode: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "empty content type", body: `{}`, contentTypes: []string{""}, wantStatus: 415, wantCode: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "repeated content type", body: `{}`, contentTypes: []string{"application/json", "application/json"}, wantStatus: 415, wantCode: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "malformed media parameter", body: `{}`, contentTypes: []string{"application/json; private-value"}, wantStatus: 415, wantCode: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "text media type", body: `{}`, contentTypes: []string{"text/plain"}, wantStatus: 415, wantCode: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "form media type", body: `name=private-name`, contentTypes: []string{"application/x-www-form-urlencoded"}, wantStatus: 415, wantCode: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "json suffix not opted in", body: `{}`, contentTypes: []string{"application/problem+json"}, wantStatus: 415, wantCode: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "unsupported charset", body: `{}`, contentTypes: []string{"application/json; charset=gbk"}, wantStatus: 415, wantCode: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "empty charset", body: `{}`, contentTypes: []string{"application/json; charset=\"\""}, wantStatus: 415, wantCode: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "compressed body", body: `private-compressed-body`, encodings: []string{"gzip"}, wantStatus: 415, wantCode: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "multiple content encoding", body: `{}`, encodings: []string{"identity", "identity"}, wantStatus: 415, wantCode: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "empty content encoding", body: `{}`, encodings: []string{""}, wantStatus: 415, wantCode: "UNSUPPORTED_MEDIA_TYPE"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			var logs bytes.Buffer
			called := false
			var received jsonBindingInput
			router := NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
			router.POST("/test-json", func(requestContext *gin.Context) {
				// 先解析，再执行后续逻辑；失败时立即 return，不能使用部分填入的 DTO。
				var input jsonBindingInput
				if !binding.ReadJSON(requestContext, &input) {
					return
				}
				called, received = true, input
				requestContext.Status(http.StatusNoContent)
			})
			request := httptest.NewRequest(http.MethodPost, "/test-json?token=private-query", strings.NewReader(testCase.body))
			contentTypes := testCase.contentTypes
			if contentTypes == nil {
				contentTypes = []string{"application/json"}
			}
			for _, contentType := range contentTypes {
				request.Header.Add("Content-Type", contentType)
			}
			for _, encoding := range testCase.encodings {
				request.Header.Add("Content-Encoding", encoding)
			}
			request.Header.Set("Origin", "http://127.0.0.1:5173")
			request.Header.Set("Cookie", "session=private-cookie")
			request.Header.Set("X-Request-ID", "28EE758A-0727-42DE-AE7D-3BB33155E148")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			// 2. 核对解析与短路结果，所有响应仍沿用 CORS、请求 ID 和完成日志。
			if response.Code != testCase.wantStatus || called != (testCase.wantStatus == 204) {
				t.Fatalf("status=%d called=%v body=%s", response.Code, called, response.Body)
			}
			if called && (received.Name != testCase.wantName || received.Count != testCase.wantCount) {
				t.Fatalf("解析后的 DTO 不正确: %+v", received)
			}
			requestID := response.Header().Get("X-Request-ID")
			if requestID != "28ee758a-0727-42de-ae7d-3bb33155e148" || response.Header().Get("Access-Control-Allow-Origin") != "http://127.0.0.1:5173" || response.Header().Get("Access-Control-Expose-Headers") != "X-Request-ID, Retry-After" {
				t.Fatal("请求 ID 或 CORS 约定丢失")
			}
			if !called {
				var body map[string]any
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				detail, valid := body["error"].(map[string]any)
				if !valid || len(body) != 2 || len(detail) != 3 || detail["code"] != testCase.wantCode || body["request_id"] != requestID || detail["message"] == "" {
					t.Fatal("统一输入错误结构不正确")
				}
				if details, exists := detail["details"]; !exists || details != nil {
					t.Fatal("details 必须为 null")
				}
				if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Content-Type") != "application/json; charset=utf-8" {
					t.Fatal("输入错误缺少 JSON 或禁止缓存头")
				}
			}
			var record map[string]any
			if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			level := "INFO"
			if !called {
				level = "WARN"
			}
			if record["request_id"] != requestID || record["status"] != float64(response.Code) || record["level"] != level {
				t.Fatal("输入错误与完成日志未关联")
			}
			if strings.Contains(response.Body.String(), "private-") || strings.Contains(logs.String(), "private-") {
				t.Fatal("响应或日志泄漏原始输入")
			}
		})
	}
}

// trackedJSONBody 记录读取字节及关闭状态，验证上限不只是比较声明长度。
type trackedJSONBody struct {
	reader    io.Reader
	bytesRead int
	closed    bool
}

func (body *trackedJSONBody) Read(buffer []byte) (int, error) {
	count, err := body.reader.Read(buffer)
	body.bytesRead += count
	return count, err
}

func (body *trackedJSONBody) Close() error { body.closed = true; return nil }

func TestReadJSONActualByteLimit(t *testing.T) {
	// 1. 同时覆盖准确边界、未知/误填长度、尾随空白和多字节字符。
	limit := int(binding.MaxJSONBodyBytes)
	exactObject := `{"name":"` + strings.Repeat("a", limit-len(`{"name":""}`)) + `"}`
	for _, testCase := range []struct {
		name, body    string
		contentLength int64
		status        int
	}{
		{name: "below limit", body: exactObject[:len(exactObject)-3] + `"}`, contentLength: -1, status: 204},
		{name: "exact limit", body: exactObject, contentLength: int64(limit), status: 204},
		{name: "one byte over unknown length", body: exactObject + " ", contentLength: -1, status: 413},
		{name: "one byte over declared small", body: exactObject + " ", contentLength: 2, status: 413},
		{name: "trailing whitespace over limit", body: `{}` + strings.Repeat(" ", limit), contentLength: -1, status: 413},
		{name: "invalid syntax also over limit", body: strings.Repeat("x", limit+100), contentLength: -1, status: 413},
		{name: "utf8 bytes over limit", body: `{"name":"` + strings.Repeat("中", limit/3) + `"}`, contentLength: -1, status: 413},
		{name: "declared large but actual small", body: `{}`, contentLength: int64(limit + 1), status: 204},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			called := false
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.POST("/test-json", func(requestContext *gin.Context) {
				var input jsonBindingInput
				if !binding.ReadJSON(requestContext, &input) {
					return
				}
				called = true
				requestContext.Status(204)
			})
			body := &trackedJSONBody{reader: strings.NewReader(testCase.body)}
			request := httptest.NewRequest("POST", "/test-json", nil)
			request.Body, request.ContentLength = body, testCase.contentLength
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != testCase.status || called != (testCase.status == 204) || !body.closed || body.bytesRead > limit+1 {
				t.Fatalf("status=%d called=%v closed=%v bytes=%d", response.Code, called, body.closed, body.bytesRead)
			}
			if testCase.status == 413 {
				var decoded struct {
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil || decoded.Error.Code != "REQUEST_BODY_TOO_LARGE" {
					t.Fatal("超限没有统一为 REQUEST_BODY_TOO_LARGE")
				}
			}
		})
	}
}

type failedJSONReader struct{}

func (failedJSONReader) Read([]byte) (int, error) { return 0, errors.New("private-reader-error") }

func TestReadJSONReadFailureAndInvalidDestination(t *testing.T) {
	// 1. 传输错误属于 400；开发者传错目标属于程序错误，保留已有 500 恢复语义。
	for _, testCase := range []struct {
		name        string
		reader      io.Reader
		destination any
		status      int
	}{
		{name: "read failure", reader: failedJSONReader{}, destination: &jsonBindingInput{}, status: 400},
		{name: "nil destination", reader: strings.NewReader(`{}`), status: 500},
		{name: "nil typed pointer", reader: strings.NewReader(`{}`), destination: (*jsonBindingInput)(nil), status: 500},
		{name: "non pointer destination", reader: strings.NewReader(`{}`), destination: jsonBindingInput{}, status: 500},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var logs bytes.Buffer
			body := &trackedJSONBody{reader: testCase.reader}
			router := NewRouter(logging.New(&logs), nil, func(context.Context) bool { return true })
			router.POST("/test-json", func(requestContext *gin.Context) {
				if binding.ReadJSON(requestContext, testCase.destination) {
					t.Error("错误输入被当成成功")
				}
			})
			request := httptest.NewRequest("POST", "/test-json", nil)
			request.Body = body
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != testCase.status || !body.closed {
				t.Fatalf("status=%d closed=%v", response.Code, body.closed)
			}
			if strings.Contains(response.Body.String(), "private-") || strings.Contains(logs.String(), "private-") {
				t.Fatal("读取错误被回显")
			}
		})
	}
}

func TestReadJSONChunkedOverRealHTTP(t *testing.T) {
	// 1. 由未知长度 Reader 发起真实分块请求，确保服务器按实际字节判断。
	router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
	router.POST("/test-json", func(requestContext *gin.Context) {
		if requestContext.Request.ContentLength != -1 || len(requestContext.Request.TransferEncoding) != 1 || requestContext.Request.TransferEncoding[0] != "chunked" {
			t.Error("请求没有按未知长度的分块传输到达服务器")
		}
		var input jsonBindingInput
		if !binding.ReadJSON(requestContext, &input) {
			return
		}
		requestContext.Status(204)
	})
	server := httptest.NewServer(router)
	defer server.Close()
	for _, testCase := range []struct {
		body, contentType string
		status            int
	}{
		{body: `{"name":"notes"}`, contentType: "application/json", status: 204},
		{body: `{"count":"private-wrong-type"}`, contentType: "application/json", status: 400},
		{body: `{}` + strings.Repeat(" ", int(binding.MaxJSONBodyBytes)), contentType: "application/json", status: 413},
		{body: `{}`, contentType: "text/plain", status: 415},
	} {
		// 隐藏 strings.Reader 的长度接口，使 net/http 使用 chunked 传输。
		request, err := http.NewRequest("POST", server.URL+"/test-json", struct{ io.Reader }{strings.NewReader(testCase.body)})
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", testCase.contentType)
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, readError := io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if readError != nil || response.StatusCode != testCase.status {
			t.Fatalf("真实分块传输 status=%d error=%v", response.StatusCode, readError)
		}
	}
}

func TestReadJSONNilBody(t *testing.T) {
	// 1. 手工构造的请求可能没有 Body，读取器应返回空正文错误，而不是 panic。
	router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
	router.POST("/test-json", func(requestContext *gin.Context) {
		var input jsonBindingInput
		if binding.ReadJSON(requestContext, &input) {
			t.Error("空正文被当成成功")
		}
	})
	request := httptest.NewRequest("POST", "/test-json", nil)
	request.Body = nil
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != 400 {
		t.Fatalf("nil 正文状态=%d", response.Code)
	}
}
