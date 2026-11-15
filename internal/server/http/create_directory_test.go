package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

func TestCreateDirectoryHandlerFlow(t *testing.T) {
	for _, testCase := range []struct {
		name, body, code, field string
		status, calls, cancelOn int
		serviceError            error
	}{
		{name: "ordinary", status: 201, calls: 1},
		{name: "trimmed name", status: 201, calls: 1},
		{name: "null color", status: 201, calls: 1},
		{name: "empty color", status: 201, calls: 1},
		{name: "maximum ids", status: 201, calls: 1},
		{name: "missing actor", status: 500, code: "INTERNAL_ERROR"},
		{name: "missing key before json", status: 400, code: "INVALID_ARGUMENT", field: "Idempotency-Key", body: `{"private-broken":`},
		{name: "duplicate key", status: 400, code: "INVALID_ARGUMENT", field: "Idempotency-Key"},
		{name: "merged key", status: 400, code: "INVALID_ARGUMENT", field: "Idempotency-Key"},
		{name: "invalid json", status: 400, code: "INVALID_ARGUMENT", body: `{"private-broken":`},
		{name: "unknown field", status: 400, code: "INVALID_ARGUMENT", body: `{"parent_id":"11","name":"资料","user_id":"99"}`},
		{name: "numeric parent", status: 400, code: "INVALID_ARGUMENT", body: `{"parent_id":11,"name":"资料"}`},
		{name: "zero parent", status: 400, code: "INVALID_ARGUMENT", field: "parent_id", body: `{"parent_id":"0","name":"资料"}`},
		{name: "missing name", status: 400, code: "INVALID_ARGUMENT", field: "name", body: `{"parent_id":"11"}`},
		{name: "invalid color dto", status: 400, code: "INVALID_ARGUMENT", field: "color_key", body: `{"parent_id":"11","name":"资料","color_key":"绿色"}`},
		{name: "unsupported type", status: 415, code: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "oversized body", status: 413, code: "REQUEST_BODY_TOO_LARGE"},
		{name: "invalid name", status: 400, calls: 1, code: "INVALID_NAME", field: "name", serviceError: domain.ErrInvalidEntryName},
		{name: "invalid color service", status: 400, calls: 1, code: "INVALID_ARGUMENT", field: "color_key", serviceError: domain.ErrInvalidDirectoryColorKey},
		{name: "file parent", status: 400, calls: 1, code: "INVALID_ARGUMENT", field: "parent_id", serviceError: domain.ErrFileEntryNotDirectory},
		{name: "depth exceeded", status: 400, calls: 1, code: "INVALID_ARGUMENT", field: "parent_id", serviceError: domain.ErrFileEntryDepthExceeded},
		{name: "hidden parent", status: 404, calls: 1, code: "ENTRY_NOT_FOUND", serviceError: domain.ErrFileEntryNotFound},
		{name: "name conflict", status: 409, calls: 1, code: "NAME_CONFLICT", serviceError: domain.ErrEntryNameConflict},
		{name: "request conflict", status: 409, calls: 1, code: "IDEMPOTENCY_CONFLICT", serviceError: domain.ErrIdempotencyRequestConflict},
		{name: "expired record", status: 409, calls: 1, code: "IDEMPOTENCY_EXPIRED", serviceError: domain.ErrIdempotencyRecordExpired},
		{name: "disabled", status: 403, calls: 1, code: "ACCOUNT_DISABLED", serviceError: domain.ErrAccountDisabled},
		{name: "revoked session", status: 401, calls: 1, code: "SESSION_REVOKED", serviceError: domain.ErrSessionRevoked},
		{name: "expired session", status: 401, calls: 1, code: "SESSION_REVOKED", serviceError: domain.ErrSessionExpired},
		{name: "revoked device", status: 401, calls: 1, code: "SESSION_REVOKED", serviceError: domain.ErrDeviceRevoked},
		{name: "missing account", status: 401, calls: 1, code: "AUTH_REQUIRED", serviceError: domain.ErrAccountNotFound},
		{name: "duplicate storage fact", status: 500, calls: 1, code: "INTERNAL_ERROR", serviceError: domain.ErrIdempotencyRecordAlreadyExists},
		{name: "storage failure", status: 500, calls: 1, code: "INTERNAL_ERROR", serviceError: errors.New("private-storage-failure")},
		{name: "nil result", status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "malformed result", status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "wrong parent result", status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "wrong name result", status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "wrong color result", status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "missing color result", status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "unexpected color result", status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "cancel before entry", status: 500, code: "INTERNAL_ERROR"},
		{name: "cancel before service", cancelOn: 2, status: 500, code: "INTERNAL_ERROR"},
		{name: "cancel after service", status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "cancel with business error", status: 500, calls: 1, code: "INTERNAL_ERROR", serviceError: domain.ErrEntryNameConflict},
		{name: "cancel at delivery", cancelOn: 4, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "cancel before write", cancelOn: 5, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "service deadline", status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "service panic", status: 500, calls: 1, code: "INTERNAL_ERROR"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 构造输入与返回快照，保持服务参数和响应核对独立。
			actor := domain.Actor{UserID: 7, DeviceID: 13, SessionID: 17}
			data := createDirectorySuccessFixture(t)
			parentID, rawName, key := uint64(11), "学习资料 🌱", "Request-007"
			green := "green"
			var color *string = &green
			switch testCase.name {
			case "trimmed name":
				rawName = "  学习资料 🌱\u3000"
			case "null color":
				color, data.Entry.ColorKey = nil, nil
			case "empty color":
				green, *data.Entry.ColorKey = "", ""
			case "maximum ids":
				actor = domain.Actor{UserID: ^uint64(0), DeviceID: ^uint64(0), SessionID: ^uint64(0)}
				parentID, data.Entry.ParentID = ^uint64(0), "18446744073709551615"
			case "wrong parent result":
				data.Entry.ParentID = "12"
			case "wrong name result":
				data.Entry.Name = "private-wrong-name"
			case "wrong color result":
				*data.Entry.ColorKey = "sand"
			case "missing color result":
				data.Entry.ColorKey = nil
			case "unexpected color result":
				color = nil
			}
			encodedRequest, err := json.Marshal(dto.CreateDirectoryRequest{ParentID: strconv.FormatUint(parentID, 10), Name: rawName, ColorKey: color})
			if err != nil {
				t.Fatal(err)
			}
			requestBody := string(encodedRequest)
			if testCase.body != "" {
				requestBody = testCase.body
			}
			if testCase.name == "oversized body" {
				requestBody = strings.Repeat(" ", (1<<20)+1)
			}
			result, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			if testCase.name == "nil result" {
				result = nil
			}
			if testCase.name == "malformed result" {
				result = []byte(`{"entry":null}`)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if testCase.name == "cancel before entry" {
				cancel()
			}
			if testCase.name == "service deadline" {
				var release context.CancelFunc
				ctx, release = context.WithTimeout(ctx, 20*time.Millisecond)
				defer release()
			}
			var logs bytes.Buffer
			var expectedContext context.Context
			calls, downstream := 0, 0
			router := NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
			create := handler.CreateDirectory(func(received context.Context, identity domain.Actor, parent uint64, name string, actualColor *string, actualKey string) (json.RawMessage, error) {
				calls++
				if received != expectedContext || identity != actor || parent != parentID || name != rawName || !reflect.DeepEqual(actualColor, color) || actualKey != key {
					t.Error("服务输入与已认证身份、正文或幂等键不一致")
				}
				if testCase.name == "cancel after service" || testCase.name == "cancel with business error" {
					cancel()
				}
				if testCase.name == "service deadline" {
					<-received.Done()
					return nil, received.Err()
				}
				if testCase.name == "service panic" {
					panic("private-service-panic")
				}
				if testCase.serviceError != nil {
					return nil, fmt.Errorf("private-service-error: %w", testCase.serviceError)
				}
				return result, nil
			})
			router.POST("/test-create-directory", func(requestContext *gin.Context) {
				if testCase.name != "missing actor" {
					requestContext.Request = requestContext.Request.WithContext(requestcontext.WithActor(requestContext.Request.Context(), actor))
				}
				if testCase.cancelOn != 0 {
					requestContext.Request = requestContext.Request.WithContext(&cancelUserSessionsContext{Context: requestContext.Request.Context(), cancel: cancel, cancelOn: testCase.cancelOn})
				}
				expectedContext = requestContext.Request.Context()
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
			}, create, func(*gin.Context) { downstream++ })
			request := httptest.NewRequest("POST", "/test-create-directory?note=private-query", strings.NewReader(requestBody)).WithContext(ctx)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Origin", "http://127.0.0.1:5173")
			request.Header.Set("Idempotency-Key", " \t"+key+"\t ")
			request.Header.Set("Authorization", "Bearer private-access")
			request.AddCookie(&http.Cookie{Name: handler.RefreshCookieName, Value: "private-refresh"})
			switch testCase.name {
			case "missing key before json":
				request.Header.Del("Idempotency-Key")
			case "duplicate key":
				request.Header.Add("Idempotency-Key", key)
			case "merged key":
				request.Header.Set("Idempotency-Key", "private-first,private-second")
			case "unsupported type":
				request.Header.Set("Content-Type", "text/plain")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			// 2. 核对服务调用次数、处理结束、统一头与完整成功或错误结构。
			if response.Code != testCase.status || calls != testCase.calls || downstream != 0 || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" ||
				response.Header().Get("Content-Type") != "application/json; charset=utf-8" || response.Header().Get("Content-Encoding") != "" || len(response.Header().Values("Set-Cookie")) != 0 {
				t.Fatal("创建入口状态、调用次数或响应头错误", response.Code, calls)
			}
			requestID := response.Header().Get("X-Request-ID")
			if testCase.status == 201 {
				var body dto.CreateDirectoryResponse
				if json.Unmarshal(response.Body.Bytes(), &body) != nil || !reflect.DeepEqual(body.Data, *data) || body.RequestID == "" || body.RequestID != requestID || response.Header().Get("Content-Length") != strconv.Itoa(response.Body.Len()) {
					t.Fatal("创建成功完整响应错误")
				}
			} else {
				var body dto.ErrorResponse
				if json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Error.Code != testCase.code || body.RequestID == "" || body.RequestID != requestID || response.Header().Get("Content-Length") != "" {
					t.Fatal("创建错误正文或关联错误", response.Body.String())
				}
				if testCase.field == "" {
					if body.Error.Details != nil {
						t.Fatal("普通错误 details 非空")
					}
				} else if len(body.Error.Details) != 1 || body.Error.Details["field"] != testCase.field {
					t.Fatal("错误字段定位错误")
				}
				if body.Error.Message == "" {
					t.Fatal("错误消息缺失")
				}
			}
			if (testCase.status == 401) != (response.Header().Get("WWW-Authenticate") != "") {
				t.Fatal("认证提示与状态不符")
			}

			// 3. 关联完成日志，保护原始请求和创建正文。
			var completed map[string]any
			if json.Unmarshal(logs.Bytes(), &completed) != nil || completed["request_id"] != requestID || completed["status"] != float64(testCase.status) || completed["path"] != "/test-create-directory" {
				t.Fatal("创建请求完成日志错误")
			}
			for _, marker := range []string{"private-query", "private-access", "private-refresh", "private-first", "private-service", "private-storage", "private-wrong", "user_id", "Request-007"} {
				if strings.Contains(response.Body.String(), marker) || strings.Contains(logs.String(), marker) {
					t.Fatal("响应或日志包含原始请求/内部数据")
				}
			}
			if strings.Contains(logs.String(), "学习资料") {
				t.Fatal("完成日志包含创建名称")
			}
		})
	}
}

func TestCreateDirectoryHandlerAuthorization(t *testing.T) {
	for _, mode := range []string{"missing bearer", "cookie only", "query only", "invalid bearer", "preflight", "forbidden origin"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			router := NewRouter(logging.New(io.Discard), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
			router.POST("/test-create-directory", middleware.RequireAuthorization(func(context.Context, string) (*domain.Actor, error) { return nil, domain.ErrInvalidAccessToken }, handler.AuthorizationFailure), handler.CreateDirectory(func(context.Context, domain.Actor, uint64, string, *string, string) (json.RawMessage, error) {
				calls++
				return nil, nil
			}))
			request := httptest.NewRequest("POST", "/test-create-directory?access_token=private-query", strings.NewReader(`{"private-broken":`))
			request.Header.Set("Origin", "http://127.0.0.1:5173")
			status, code := 401, "AUTH_REQUIRED"
			switch mode {
			case "cookie only":
				request.AddCookie(&http.Cookie{Name: handler.RefreshCookieName, Value: "private-cookie"})
			case "invalid bearer":
				request.Header.Set("Authorization", "Bearer private-access")
			case "preflight":
				request.Method = "OPTIONS"
				request.Header.Set("Access-Control-Request-Method", "POST")
				request.Header.Set("Access-Control-Request-Headers", "Authorization,Content-Type,Idempotency-Key")
				status = 204
			case "forbidden origin":
				request.Header.Set("Origin", "http://private-origin.example")
				status, code = 403, "CORS_FORBIDDEN"
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != status || calls != 0 {
				t.Fatal("认证或来源未先于参数和服务处理")
			}
			if status == 204 {
				if response.Body.Len() != 0 {
					t.Fatal("预检存在正文")
				}
			} else {
				var body dto.ErrorResponse
				if json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Error.Code != code || body.Error.Details != nil {
					t.Fatal("认证或来源错误结构不符")
				}
			}
		})
	}
}

func TestCreateDirectoryHandlerCommittedResponse(t *testing.T) {
	for _, headersOnly := range []bool{false, true} {
		calls := 0
		router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
		router.POST("/test-create-directory", func(requestContext *gin.Context) {
			if headersOnly {
				requestContext.Status(202)
				requestContext.Writer.WriteHeaderNow()
			} else {
				requestContext.String(202, "already-written")
			}
		}, handler.CreateDirectory(func(context.Context, domain.Actor, uint64, string, *string, string) (json.RawMessage, error) {
			calls++
			return nil, nil
		}), func(*gin.Context) { t.Error("已发送响应进入下游") })
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest("POST", "/test-create-directory", nil))
		expected := "already-written"
		if headersOnly {
			expected = ""
		}
		if response.Code != 202 || response.Body.String() != expected || calls != 0 {
			t.Fatal("已发送响应被改写或调用服务")
		}
	}
}

func TestCreateDirectoryHandlerDependency(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("缺少服务仍装配成功")
		}
	}()
	handler.CreateDirectory(nil)
}
