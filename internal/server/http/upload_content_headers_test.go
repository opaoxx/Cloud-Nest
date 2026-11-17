package http

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// uploadContentTrackedBody 记录调用次数，验证头校验先于任何正文访问。
type uploadContentTrackedBody struct {
	reader io.Reader
	reads  int
	closes int
}

func (body *uploadContentTrackedBody) Read(buffer []byte) (int, error) {
	body.reads++
	return body.reader.Read(buffer)
}

func (body *uploadContentTrackedBody) Close() error { body.closes++; return nil }

func TestValidateUploadContentHeadersPolicy(t *testing.T) {
	for _, fixture := range []struct {
		name         string
		contentTypes []string
		encodings    []string
		accepted     bool
	}{
		{"binary", []string{"application/octet-stream"}, nil, true},
		{"mixed case", []string{"Application/Octet-Stream"}, nil, true},
		{"outer whitespace", []string{" application/octet-stream \t"}, nil, true},
		{"valid parameter", []string{"application/octet-stream; profile=private-media-parameter"}, nil, true},
		{"quoted parameter", []string{`application/octet-stream; name="学习资料.bin"`}, nil, true},
		{"binary charset parameter", []string{"application/octet-stream; charset=gbk"}, nil, true},
		{"identity", []string{"application/octet-stream"}, []string{"identity"}, true},
		{"identity case and whitespace", []string{"application/octet-stream"}, []string{" IDENTITY \t"}, true},
		{"missing type", nil, nil, false},
		{"empty type", []string{""}, nil, false},
		{"whitespace type", []string{" \t"}, nil, false},
		{"duplicate type", []string{"application/octet-stream", "application/octet-stream"}, nil, false},
		{"mixed duplicate type", []string{"application/octet-stream", "application/json"}, nil, false},
		{"comma joined type", []string{"application/octet-stream, application/octet-stream"}, nil, false},
		{"JSON", []string{"application/json"}, nil, false},
		{"image", []string{"image/png"}, nil, false},
		{"form", []string{"multipart/form-data; boundary=private-media-parameter"}, nil, false},
		{"text", []string{"text/plain"}, nil, false},
		{"missing parameter value", []string{"application/octet-stream; private-media-parameter"}, nil, false},
		{"broken quote", []string{`application/octet-stream; name="private-media-parameter`}, nil, false},
		{"conflicting parameters", []string{"application/octet-stream; profile=a; profile=b"}, nil, false},
		{"invalid token", []string{"application/octet-stream\x00"}, nil, false},
		{"empty encoding", []string{"application/octet-stream"}, []string{""}, false},
		{"whitespace encoding", []string{"application/octet-stream"}, []string{" \t"}, false},
		{"gzip", []string{"application/octet-stream"}, []string{"gzip"}, false},
		{"br", []string{"application/octet-stream"}, []string{"br"}, false},
		{"deflate", []string{"application/octet-stream"}, []string{"deflate"}, false},
		{"duplicate identity", []string{"application/octet-stream"}, []string{"identity", "identity"}, false},
		{"encoding list", []string{"application/octet-stream"}, []string{"identity, gzip"}, false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 校验运行于实际公共中间件链，正文包含任意二进制值。
			var logs bytes.Buffer
			bodyText := "private-upload-body\x00\xff\xfe"
			body := &uploadContentTrackedBody{reader: strings.NewReader(bodyText)}
			businessCalls, downstreamCalls := 0, 0
			router := NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
			router.PUT("/test-upload-headers", func(requestContext *gin.Context) {
				originalBody, originalHeaders, originalLength := requestContext.Request.Body, requestContext.Request.Header.Clone(), requestContext.Request.ContentLength
				if !fixture.accepted {
					requestContext.Header("Content-Length", "1")
					requestContext.Header("Content-Encoding", "gzip")
				}
				accepted := binding.ValidateUploadContentHeaders(requestContext)
				if accepted != fixture.accepted || body.reads != 0 || body.closes != 0 || requestContext.Request.Body != originalBody ||
					requestContext.Request.ContentLength != originalLength || !reflect.DeepEqual(requestContext.Request.Header, originalHeaders) {
					t.Error("头校验读取/关闭/替换正文或改动请求事实")
				}
				if !accepted {
					return
				}
				businessCalls++
				actual, err := io.ReadAll(requestContext.Request.Body)
				if err != nil || string(actual) != bodyText {
					t.Error("后续读取未取得完整原始字节", err)
				}
				requestContext.Status(http.StatusNoContent)
			}, func(*gin.Context) { downstreamCalls++ })
			request := httptest.NewRequest(http.MethodPut, "/test-upload-headers?token=private-upload-query", nil)
			request.Body, request.ContentLength = body, -1
			for _, value := range fixture.contentTypes {
				request.Header.Add("Content-Type", value)
			}
			for _, value := range fixture.encodings {
				request.Header.Add("Content-Encoding", value)
			}
			request.Header.Set("Origin", "http://127.0.0.1:5173")
			request.Header.Set("X-Request-ID", "28EE758A-0727-42DE-AE7D-3BB33155E148")
			request.Header.Set("Cookie", "session=private-upload-cookie")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			wantStatus, wantCalls := http.StatusUnsupportedMediaType, 0
			if fixture.accepted {
				wantStatus, wantCalls = http.StatusNoContent, 1
			}
			if response.Code != wantStatus || businessCalls != wantCalls || downstreamCalls != wantCalls {
				t.Fatal("媒体校验未正确短路", response.Code, businessCalls, downstreamCalls)
			}
			// 2. 错误沿统一 DTO 交付，清理旧正文头并保留 CORS/请求 ID。
			requestID := response.Header().Get("X-Request-ID")
			if requestID != "28ee758a-0727-42de-ae7d-3bb33155e148" || response.Header().Get("Access-Control-Allow-Origin") != "http://127.0.0.1:5173" {
				t.Fatal("媒体校验交付丢失请求 ID 或来源头")
			}
			if !fixture.accepted {
				var result dto.ErrorResponse
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.RequestID != requestID || result.Error.Code != "UNSUPPORTED_MEDIA_TYPE" ||
					result.Error.Message != "请求必须使用 application/octet-stream，且不压缩正文" || result.Error.Details != nil ||
					response.Header().Get("Content-Length") != "" || response.Header().Get("Content-Encoding") != "" ||
					response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" || response.Header().Get("Content-Type") != "application/json; charset=utf-8" {
					t.Fatal("上传媒体错误格式或响应头不正确", err)
				}
			}
			var record map[string]any
			if err := json.Unmarshal(logs.Bytes(), &record); err != nil || record["request_id"] != requestID || record["status"] != float64(wantStatus) {
				t.Fatal("完成日志关联错误", err)
			}
			for _, private := range []string{"private-upload-body", "private-upload-query", "private-upload-cookie", "private-media-parameter"} {
				if strings.Contains(logs.String(), private) || strings.Contains(response.Body.String(), private) {
					t.Fatal("媒体错误或日志泄露原始输入")
				}
			}
		})
	}
}

func TestValidateUploadContentHeadersBodyOwnership(t *testing.T) {
	for _, fixture := range []struct {
		name    string
		length  int64
		nilBody bool
	}{
		{"zero", 0, false}, {"unknown", -1, false}, {"above JSON limit", binding.MaxJSONBodyBytes + 1, false},
		{"above SINGLE limit", int64(domain.MaxSingleUploadSizeBytes) + 1, false}, {"maximum declared length", int64(^uint64(0) >> 1), false}, {"nil body", 0, true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			requestContext, _ := gin.CreateTestContext(response)
			request := httptest.NewRequest(http.MethodPut, "/test-upload-headers", nil)
			body := &uploadContentTrackedBody{reader: failedJSONReader{}}
			request.Body, request.ContentLength = body, fixture.length
			if fixture.nilBody {
				request.Body = nil
			}
			request.Header.Set("Content-Type", "application/octet-stream")
			requestContext.Request = request
			before := request.Body
			if !binding.ValidateUploadContentHeaders(requestContext) || body.reads != 0 || body.closes != 0 || request.Body != before || request.ContentLength != fixture.length || requestContext.Writer.Written() {
				t.Fatal("媒体校验混入正文访问、长度判定或成功响应")
			}
		})
	}
}

func TestValidateUploadContentHeadersCommitted(t *testing.T) {
	for _, mode := range []string{"written", "flushed headers", "aborted"} {
		t.Run(mode, func(t *testing.T) {
			response := httptest.NewRecorder()
			requestContext, _ := gin.CreateTestContext(response)
			requestContext.Request = httptest.NewRequest(http.MethodPut, "/test-upload-headers", nil)
			body := &uploadContentTrackedBody{reader: strings.NewReader("private-upload-body")}
			requestContext.Request.Body = body
			requestContext.Header("X-Keep", "kept")
			switch mode {
			case "written":
				requestContext.String(http.StatusAccepted, "kept")
			case "flushed headers":
				requestContext.Status(http.StatusNoContent)
				requestContext.Writer.WriteHeaderNow()
			case "aborted":
				requestContext.Abort()
			}
			beforeHeaders, beforeBody, beforeCode := response.Header().Clone(), response.Body.String(), response.Code
			if binding.ValidateUploadContentHeaders(requestContext) || !requestContext.IsAborted() || body.reads != 0 || body.closes != 0 ||
				!reflect.DeepEqual(beforeHeaders, response.Header()) || beforeBody != response.Body.String() || beforeCode != response.Code {
				t.Fatal("已结束请求被追加响应或继续处理")
			}
		})
	}
}

func TestValidateUploadContentHeadersAuthorizedFlow(t *testing.T) {
	for _, fixture := range []struct {
		name                               string
		status, auth, path, media, prepare int
	}{
		{"allowed", 204, 1, 1, 1, 1},
		{"maximum task ID", 204, 1, 1, 1, 1},
		{"wrong media", 415, 1, 1, 1, 0},
		{"wrong encoding", 415, 1, 1, 1, 0},
		{"missing bearer", 401, 0, 0, 0, 0},
		{"invalid bearer", 401, 1, 0, 0, 0},
		{"bad task ID", 400, 1, 1, 0, 0},
		{"cancelled", 500, 0, 0, 0, 0},
		{"unknown origin", 403, 0, 0, 0, 0},
		{"preflight", 204, 0, 0, 0, 0},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 复用正式认证和路径 DTO，头校验成功后才进入准备入口。
			var logs bytes.Buffer
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			authCalls, pathCalls, mediaCalls, prepareCalls := 0, 0, 0, 0
			router := NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
			protected := middleware.RequireAuthorization(func(_ context.Context, stringToken string) (*domain.Actor, error) {
				authCalls++
				if stringToken != "fixture-access" {
					return nil, domain.ErrInvalidAccessToken
				}
				return &actor, nil
			}, handler.AuthorizationFailure)
			router.PUT("/test-upload-content/:id/content", protected, func(requestContext *gin.Context) {
				pathCalls++
				input, fieldError := dto.ParseUploadTaskPath(requestContext.Param("id"))
				if fieldError != nil {
					handler.InvalidArgument(requestContext, fieldError)
					return
				}
				mediaCalls++
				if !binding.ValidateUploadContentHeaders(requestContext) {
					return
				}
				trusted, exists := requestcontext.Actor(requestContext.Request.Context())
				if !exists || trusted != actor || (fixture.name == "maximum task ID" && input.TaskID != ^uint64(0)) {
					t.Error("准备入口未取得正确可信身份或完整任务 ID")
				}
				prepareCalls++
				requestContext.Status(http.StatusNoContent)
			})
			method, rawID := http.MethodPut, "19"
			if fixture.name == "maximum task ID" {
				rawID = "18446744073709551615"
			}
			if fixture.name == "bad task ID" {
				rawID = "not-a-task"
			}
			if fixture.name == "preflight" {
				method = http.MethodOptions
			}
			body := &uploadContentTrackedBody{reader: strings.NewReader("private-upload-body")}
			request := httptest.NewRequest(method, "/test-upload-content/"+rawID+"/content?user_id=99&token=private-upload-query", nil)
			request.Body = body
			request.Header.Set("Content-Type", "application/octet-stream")
			request.Header.Set("Authorization", "Bearer fixture-access")
			request.Header.Set("Origin", "http://127.0.0.1:5173")
			switch fixture.name {
			case "wrong media":
				request.Header.Set("Content-Type", "application/json")
			case "wrong encoding":
				request.Header.Set("Content-Encoding", "gzip")
			case "missing bearer":
				request.Header.Del("Authorization")
			case "invalid bearer":
				request.Header.Set("Authorization", "Bearer wrong-access")
			case "cancelled":
				cancelled, cancel := context.WithCancel(request.Context())
				cancel()
				request = request.WithContext(cancelled)
			case "unknown origin":
				request.Header.Set("Origin", "https://other.example")
			case "preflight":
				request.Header.Set("Access-Control-Request-Method", "PUT")
				request.Header.Set("Access-Control-Request-Headers", "authorization,content-type")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			// 2. 认证、路径、媒体和准备依次短路，所有场景保留正文所有权。
			if response.Code != fixture.status || authCalls != fixture.auth || pathCalls != fixture.path || mediaCalls != fixture.media || prepareCalls != fixture.prepare || body.reads != 0 || body.closes != 0 {
				t.Fatal("上传输入链执行顺序错误", response.Code, authCalls, pathCalls, mediaCalls, prepareCalls)
			}
			if strings.Contains(logs.String(), "fixture-access") || strings.Contains(logs.String(), "private-upload-query") || strings.Contains(logs.String(), "private-upload-body") {
				t.Fatal("上传输入链日志包含原始凭证或正文")
			}
		})
	}
}

func TestValidateUploadContentHeadersRealHTTP(t *testing.T) {
	for _, fixture := range []struct {
		name         string
		contentTypes []string
		encodings    []string
		chunked      bool
		large, empty bool
		status       int
	}{
		{"binary known length", []string{"application/octet-stream"}, nil, false, false, false, 204},
		{"binary chunked", []string{"application/octet-stream"}, nil, true, false, false, 204},
		{"empty file", []string{"application/octet-stream"}, nil, false, false, true, 204},
		{"above JSON byte limit", []string{"application/octet-stream; profile=fixture"}, nil, true, true, false, 204},
		{"identity", []string{"application/octet-stream"}, []string{"identity"}, false, false, false, 204},
		{"duplicate content type", []string{"application/octet-stream", "application/octet-stream"}, nil, false, false, false, 415},
		{"compressed", []string{"application/octet-stream"}, []string{"gzip"}, false, false, false, 415},
		{"missing content type", nil, nil, false, false, false, 415},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 系统分配端口，真实已知长度和分块传输经过同一校验入口。
			payload := []byte("private-upload-body\x00\xff\xfe")
			if fixture.empty {
				payload = nil
			}
			if fixture.large {
				payload = bytes.Repeat([]byte{0, 0xff, 0xfe, 'a'}, int(binding.MaxJSONBodyBytes)/4+6)
			}
			wantDigest := sha256.Sum256(payload)
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.PUT("/test-upload-content", func(requestContext *gin.Context) {
				if fixture.chunked && (requestContext.Request.ContentLength != -1 || !reflect.DeepEqual(requestContext.Request.TransferEncoding, []string{"chunked"})) {
					t.Error("真实请求没有采用未知长度分块传输")
				}
				tracked := &uploadContentTrackedBody{reader: requestContext.Request.Body}
				requestContext.Request.Body = tracked
				accepted := binding.ValidateUploadContentHeaders(requestContext)
				if tracked.reads != 0 || tracked.closes != 0 || accepted != (fixture.status == 204) {
					t.Error("真实头校验提前访问正文或接受状态错误")
				}
				if !accepted {
					return
				}
				// 2. 测试消费者取得完整原始字节，空文件和非 UTF-8 内容可继续处理。
				content, err := io.ReadAll(requestContext.Request.Body)
				if err != nil || len(content) != len(payload) || sha256.Sum256(content) != wantDigest {
					t.Error("原始文件内容交接错误", err)
				}
				requestContext.Status(http.StatusNoContent)
			})
			server := httptest.NewServer(router)
			defer server.Close()
			var reader io.Reader = bytes.NewReader(payload)
			if fixture.chunked {
				reader = struct{ io.Reader }{reader}
			}
			request, err := http.NewRequest(http.MethodPut, server.URL+"/test-upload-content", reader)
			if err != nil {
				t.Fatal(err)
			}
			for _, value := range fixture.contentTypes {
				request.Header.Add("Content-Type", value)
			}
			for _, value := range fixture.encodings {
				request.Header.Add("Content-Encoding", value)
			}
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, readError := io.ReadAll(response.Body)
			closeError := response.Body.Close()
			if response.StatusCode != fixture.status || readError != nil || closeError != nil || response.Header.Get("X-Request-ID") == "" {
				t.Fatal("真实媒体校验交付错误", response.StatusCode, readError, closeError)
			}
			if fixture.status == 415 {
				var result dto.ErrorResponse
				if err := json.Unmarshal(body, &result); err != nil || result.Error.Code != "UNSUPPORTED_MEDIA_TYPE" || result.RequestID != response.Header.Get("X-Request-ID") || response.Header.Get("Content-Encoding") != "" {
					t.Fatal("真实上传媒体错误不是关联当前请求的 JSON", err)
				}
			} else if len(body) != 0 {
				t.Fatal("成功测试响应应为空")
			}
		})
	}
}
