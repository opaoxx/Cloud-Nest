package http

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"math"
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

func TestValidateUploadContentLengthPolicy(t *testing.T) {
	for _, fixture := range []struct {
		name      string
		length    int64
		declared  uint64
		rawLength string
		status    int
	}{
		{"zero file", 0, 0, "", 204},
		{"one byte", 1, 1, "", 204},
		{"ordinary", 2048, 2048, "", 204},
		{"SINGLE limit", int64(domain.MaxSingleUploadSizeBytes), domain.MaxSingleUploadSizeBytes, "", 204},
		{"above JSON safe integer", 9007199254740993, 9007199254740993, "", 204},
		{"maximum signed match", math.MaxInt64, math.MaxInt64, "", 204},
		{"unknown empty", -1, 0, "", 204},
		{"unknown ordinary", -1, 2048, "", 204},
		{"unknown SINGLE limit", -1, domain.MaxSingleUploadSizeBytes, "", 204},
		{"unknown full unsigned declaration", -1, math.MaxUint64, "", 204},
		{"zero below declaration", 0, 1, "", 413},
		{"short", 3, 4, "", 413},
		{"long", 5, 4, "", 413},
		{"nonempty for empty declaration", 1, 0, "", 413},
		{"maximum signed mismatch", math.MaxInt64, domain.MaxSingleUploadSizeBytes, "", 413},
		{"unsigned conversion boundary", math.MaxInt64, uint64(1) << 63, "", 413},
		{"maximum unsigned declaration mismatch", math.MaxInt64, math.MaxUint64, "", 413},
		{"zero and full unsigned declaration", 0, math.MaxUint64, "", 413},
		{"invalid negative", -2, 4, "", 400},
		{"minimum signed", math.MinInt64, 0, "", 400},
		{"invalid negative before unsigned conversion", -2, math.MaxUint64 - 1, "", 400},
		{"parsed field matches", 4, 4, "999", 204},
		{"raw header cannot override parsed field", 5, 4, "4", 413},
		{"unknown parsed field", -1, 4, "4", 204},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 使用解析后的字段核对大小，正文故障源确保校验先于读取。
			var logs bytes.Buffer
			body := &uploadContentTrackedBody{reader: failedJSONReader{}}
			calls, downstreamCalls := 0, 0
			router := NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
			router.PUT("/test-upload-length", func(requestContext *gin.Context) {
				beforeBody, beforeHeaders, beforeLength := requestContext.Request.Body, requestContext.Request.Header.Clone(), requestContext.Request.ContentLength
				if fixture.status != 204 {
					requestContext.Header("Content-Length", "1")
					requestContext.Header("Content-Encoding", "gzip")
				}
				accepted := binding.ValidateUploadContentLength(requestContext, fixture.declared)
				if accepted != (fixture.status == 204) || body.reads != 0 || body.closes != 0 || requestContext.Request.Body != beforeBody ||
					!reflect.DeepEqual(beforeHeaders, requestContext.Request.Header) || beforeLength != requestContext.Request.ContentLength {
					t.Error("长度校验访问正文或改动请求元数据")
				}
				if !accepted {
					return
				}
				calls++
				requestContext.Status(http.StatusNoContent)
			}, func(*gin.Context) { downstreamCalls++ })
			request := httptest.NewRequest(http.MethodPut, "/test-upload-length?size_bytes=999&token=private-length-query", nil)
			request.Body, request.ContentLength = body, fixture.length
			request.Header.Set("Content-Type", "application/octet-stream")
			request.Header.Set("Origin", "http://127.0.0.1:5173")
			request.Header.Set("X-Request-ID", "28EE758A-0727-42DE-AE7D-3BB33155E148")
			request.Header.Set("Cookie", "session=private-length-cookie")
			if fixture.rawLength != "" {
				request.Header.Set("Content-Length", fixture.rawLength)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			wantCalls := 0
			if fixture.status == 204 {
				wantCalls = 1
			}
			if response.Code != fixture.status || calls != wantCalls || downstreamCalls != wantCalls {
				t.Fatal("长度校验短路或状态错误", response.Code, calls, downstreamCalls)
			}
			// 2. 固定 400/413 错误、正文头清理及日志关联逐项核对。
			requestID := response.Header().Get("X-Request-ID")
			if requestID != "28ee758a-0727-42de-ae7d-3bb33155e148" || response.Header().Get("Access-Control-Allow-Origin") != "http://127.0.0.1:5173" {
				t.Fatal("长度交付丢失请求 ID 或 CORS")
			}
			if fixture.status != 204 {
				var result dto.ErrorResponse
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				code, message := "BODY_SIZE_MISMATCH", "请求正文长度与上传声明不一致"
				if fixture.status == 400 {
					code, message = "INVALID_ARGUMENT", "请求正文长度无效"
				}
				if result.Error.Code != code || result.Error.Message != message || result.Error.Details != nil || result.RequestID != requestID ||
					response.Header().Get("Content-Length") != "" || response.Header().Get("Content-Encoding") != "" ||
					response.Header().Get("Content-Type") != "application/json; charset=utf-8" || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" {
					t.Fatal("长度错误或响应头格式错误")
				}
			}
			var record map[string]any
			if err := json.Unmarshal(logs.Bytes(), &record); err != nil || record["request_id"] != requestID || record["status"] != float64(fixture.status) {
				t.Fatal("长度交付完成日志错误", err)
			}
			if strings.Contains(logs.String(), "private-length-query") || strings.Contains(logs.String(), "private-length-cookie") || strings.Contains(response.Body.String(), "private-reader-error") {
				t.Fatal("长度交付包含原始请求输入")
			}
		})
	}
}

func TestValidateUploadContentLengthBodyAndCommitted(t *testing.T) {
	for _, mode := range []string{"nil body empty", "nil body unknown", "written", "flushed headers", "aborted"} {
		t.Run(mode, func(t *testing.T) {
			response := httptest.NewRecorder()
			requestContext, _ := gin.CreateTestContext(response)
			requestContext.Request = httptest.NewRequest(http.MethodPut, "/test-upload-length", nil)
			requestContext.Request.Body = nil
			length, declared := int64(7), uint64(4)
			accepted := false
			switch mode {
			case "nil body empty":
				length, declared, accepted = 0, 0, true
			case "nil body unknown":
				length, accepted = -1, true
			case "written":
				requestContext.String(http.StatusAccepted, "kept")
			case "flushed headers":
				requestContext.Status(http.StatusNoContent)
				requestContext.Writer.WriteHeaderNow()
			case "aborted":
				requestContext.Abort()
			}
			requestContext.Request.ContentLength = length
			beforeHeaders, beforeBody, beforeCode := response.Header().Clone(), response.Body.String(), response.Code
			if binding.ValidateUploadContentLength(requestContext, declared) != accepted || requestContext.Request.Body != nil ||
				beforeCode != response.Code || beforeBody != response.Body.String() || !reflect.DeepEqual(beforeHeaders, response.Header()) || (!accepted && !requestContext.IsAborted()) {
				t.Fatal("空正文元数据或已结束状态处理错误")
			}
		})
	}
}

func TestValidateUploadContentLengthAuthorizedFlow(t *testing.T) {
	for _, fixture := range []struct {
		name            string
		length          int64
		status          int
		lookup, prepare int
	}{
		{"known match", 4, 204, 1, 1}, {"short", 3, 413, 1, 0}, {"long", 5, 413, 1, 0},
		{"unknown", -1, 204, 1, 1}, {"empty for declared file", 0, 413, 1, 0}, {"invalid metadata", -2, 400, 1, 0},
		{"wrong media", 4, 415, 0, 0}, {"missing bearer", 4, 401, 0, 0}, {"bad task ID", 4, 400, 0, 0}, {"maximum task ID", 4, 204, 1, 1},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 任务声明由当前用户范围读取，查询参数无法替代声明。
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			lookupCalls, prepareCalls := 0, 0
			body := &uploadContentTrackedBody{reader: failedJSONReader{}}
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			protected := middleware.RequireAuthorization(func(_ context.Context, token string) (*domain.Actor, error) {
				if token != "fixture-access" {
					return nil, domain.ErrInvalidAccessToken
				}
				return &actor, nil
			}, handler.AuthorizationFailure)
			router.PUT("/test-upload-length/:id/content", protected, func(requestContext *gin.Context) {
				input, fieldError := dto.ParseUploadTaskPath(requestContext.Param("id"))
				if fieldError != nil {
					handler.InvalidArgument(requestContext, fieldError)
					return
				}
				if !binding.ValidateUploadContentHeaders(requestContext) {
					return
				}
				trusted, exists := requestcontext.Actor(requestContext.Request.Context())
				if !exists || trusted != actor || (fixture.name == "maximum task ID" && input.TaskID != math.MaxUint64) {
					t.Error("任务读取入口的身份或标识错误")
				}
				lookupCalls++
				declaredSize := uint64(4)
				if !binding.ValidateUploadContentLength(requestContext, declaredSize) {
					return
				}
				prepareCalls++
				requestContext.Status(http.StatusNoContent)
			})
			rawID := "19"
			if fixture.name == "bad task ID" {
				rawID = "0"
			}
			if fixture.name == "maximum task ID" {
				rawID = "18446744073709551615"
			}
			request := httptest.NewRequest(http.MethodPut, "/test-upload-length/"+rawID+"/content?size_bytes=999&user_id=99", nil)
			request.Body, request.ContentLength = body, fixture.length
			request.Header.Set("Content-Type", "application/octet-stream")
			if fixture.name == "wrong media" {
				request.Header.Set("Content-Type", "application/json")
			}
			if fixture.name != "missing bearer" {
				request.Header.Set("Authorization", "Bearer fixture-access")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			// 2. 长度不匹配在领取租约前结束，正文读/关计数为零。
			if response.Code != fixture.status || lookupCalls != fixture.lookup || prepareCalls != fixture.prepare || body.reads != 0 || body.closes != 0 {
				t.Fatal("已认证上传长度输入链错误", response.Code, lookupCalls, prepareCalls)
			}
		})
	}
}

func TestValidateUploadContentLengthRealHTTP(t *testing.T) {
	for _, fixture := range []struct {
		name    string
		payload []byte
		size    uint64
		chunked bool
		status  int
	}{
		{"known match", []byte{0, 0xff, 0xfe, 1}, 4, false, 204},
		{"known short", []byte{0, 0xff, 1}, 4, false, 413},
		{"known long", []byte{0, 0xff, 0xfe, 1, 2}, 4, false, 413},
		{"known empty", nil, 0, false, 204},
		{"empty mismatch", nil, 4, false, 413},
		{"chunked binary", []byte{0, 0xff, 0xfe, 1}, 4, true, 204},
		{"chunked empty", nil, 0, true, 204},
		{"above JSON limit", bytes.Repeat([]byte{0xff}, int(binding.MaxJSONBodyBytes)+7), uint64(binding.MaxJSONBodyBytes) + 7, false, 204},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 使用真实 HTTP 解析的长度字段，支持分块请求的 -1。
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.PUT("/test-upload-length", func(requestContext *gin.Context) {
				if fixture.chunked && (requestContext.Request.ContentLength != -1 || !reflect.DeepEqual(requestContext.Request.TransferEncoding, []string{"chunked"})) {
					t.Error("分块传输元数据错误")
				}
				tracked := &uploadContentTrackedBody{reader: requestContext.Request.Body}
				requestContext.Request.Body = tracked
				if !binding.ValidateUploadContentHeaders(requestContext) {
					t.Error("真实二进制媒体格式被拒绝")
					return
				}
				accepted := binding.ValidateUploadContentLength(requestContext, fixture.size)
				if accepted != (fixture.status == 204) || tracked.reads != 0 || tracked.closes != 0 {
					t.Error("真实长度校验访问正文或结果错误")
				}
				if !accepted {
					return
				}
				// 2. 测试消费者继续获得完整内容，按实际字节核对大小与摘要。
				content, err := io.ReadAll(requestContext.Request.Body)
				if err != nil || uint64(len(content)) != fixture.size || sha256.Sum256(content) != sha256.Sum256(fixture.payload) {
					t.Error("原始文件流交接错误", err)
				}
				requestContext.Status(http.StatusNoContent)
			})
			server := httptest.NewServer(router)
			defer server.Close()
			var reader io.Reader = bytes.NewReader(fixture.payload)
			if fixture.chunked {
				reader = struct{ io.Reader }{reader}
			}
			request, err := http.NewRequest(http.MethodPut, server.URL+"/test-upload-length", reader)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Content-Type", "application/octet-stream")
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, readError := io.ReadAll(response.Body)
			closeError := response.Body.Close()
			if response.StatusCode != fixture.status || readError != nil || closeError != nil {
				t.Fatal("真实长度交付错误", response.StatusCode, readError, closeError)
			}
			if fixture.status == 413 {
				var result dto.ErrorResponse
				if err := json.Unmarshal(body, &result); err != nil || result.Error.Code != "BODY_SIZE_MISMATCH" || result.RequestID != response.Header.Get("X-Request-ID") || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Pragma") != "no-cache" {
					t.Fatal("真实大小不匹配错误格式错误", err)
				}
			}
		})
	}
}
