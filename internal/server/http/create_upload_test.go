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

func TestCreateUploadHandlerFlow(t *testing.T) {
	for _, fixture := range []struct {
		name, body, code, field string
		status, calls, cancelOn int
		serviceError            error
	}{
		{name: "ordinary", status: 201, calls: 1}, {name: "trimmed name", status: 201, calls: 1}, {name: "zero bytes", status: 201, calls: 1},
		{name: "single limit", status: 201, calls: 1}, {name: "mixed SHA", status: 201, calls: 1}, {name: "maximum IDs", status: 201, calls: 1},
		{name: "missing actor", status: 500, code: "INTERNAL_ERROR"}, {name: "missing key", status: 400, code: "INVALID_ARGUMENT", field: "Idempotency-Key"},
		{name: "duplicate key", status: 400, code: "INVALID_ARGUMENT", field: "Idempotency-Key"},
		{name: "broken JSON", body: `{"private-broken":`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "missing parent", body: `{"name":"资料","size_bytes":0}`, status: 400, code: "INVALID_ARGUMENT", field: "parent_id"},
		{name: "missing name", body: `{"parent_id":"11","size_bytes":0}`, status: 400, code: "INVALID_ARGUMENT", field: "name"},
		{name: "missing size", body: `{"parent_id":"11","name":"资料"}`, status: 400, code: "INVALID_ARGUMENT", field: "size_bytes"},
		{name: "bad SHA", body: `{"parent_id":"11","name":"资料","size_bytes":0,"sha256":"private-hash"}`, status: 400, code: "INVALID_ARGUMENT", field: "sha256"},
		{name: "unknown identity", body: `{"parent_id":"11","name":"资料","size_bytes":0,"user_id":99}`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "unsupported type", status: 415, code: "UNSUPPORTED_MEDIA_TYPE"}, {name: "oversized JSON", status: 413, code: "REQUEST_BODY_TOO_LARGE"},
		{name: "oversize declaration", status: 413, calls: 1, code: "FILE_TOO_LARGE", field: "size_bytes", serviceError: domain.ErrUploadFileTooLarge},
		{name: "maximum size declaration", status: 413, calls: 1, code: "FILE_TOO_LARGE", field: "size_bytes", serviceError: domain.ErrUploadFileTooLarge},
		{name: "invalid name service", status: 400, calls: 1, code: "INVALID_NAME", field: "name", serviceError: domain.ErrInvalidEntryName},
		{name: "invalid parent service", status: 400, calls: 1, code: "INVALID_ARGUMENT", field: "parent_id", serviceError: domain.ErrInvalidUploadParentID},
		{name: "file parent", status: 400, calls: 1, code: "INVALID_ARGUMENT", field: "parent_id", serviceError: domain.ErrFileEntryNotDirectory},
		{name: "depth", status: 400, calls: 1, code: "INVALID_ARGUMENT", field: "parent_id", serviceError: domain.ErrFileEntryDepthExceeded},
		{name: "hidden parent", status: 404, calls: 1, code: "ENTRY_NOT_FOUND", serviceError: domain.ErrFileEntryNotFound},
		{name: "quota", status: 507, calls: 1, code: "QUOTA_EXCEEDED", serviceError: domain.ErrQuotaExceeded},
		{name: "conflict", status: 409, calls: 1, code: "IDEMPOTENCY_CONFLICT", serviceError: domain.ErrIdempotencyRequestConflict},
		{name: "expired record", status: 409, calls: 1, code: "IDEMPOTENCY_EXPIRED", serviceError: domain.ErrIdempotencyRecordExpired},
		{name: "disabled", status: 403, calls: 1, code: "ACCOUNT_DISABLED", serviceError: domain.ErrAccountDisabled},
		{name: "revoked session", status: 401, calls: 1, code: "SESSION_REVOKED", serviceError: domain.ErrSessionRevoked},
		{name: "expired session", status: 401, calls: 1, code: "SESSION_REVOKED", serviceError: domain.ErrSessionExpired},
		{name: "revoked device", status: 401, calls: 1, code: "SESSION_REVOKED", serviceError: domain.ErrDeviceRevoked},
		{name: "missing account", status: 401, calls: 1, code: "AUTH_REQUIRED", serviceError: domain.ErrAccountNotFound},
		{name: "duplicate result", status: 500, calls: 1, code: "INTERNAL_ERROR", serviceError: domain.ErrIdempotencyRecordAlreadyExists},
		{name: "object conflict", status: 500, calls: 1, code: "INTERNAL_ERROR", serviceError: domain.ErrStoredObjectLocationConflict},
		{name: "task object conflict", status: 500, calls: 1, code: "INTERNAL_ERROR", serviceError: domain.ErrUploadTaskStagedObjectConflict},
		{name: "unknown failure", status: 500, calls: 1, code: "INTERNAL_ERROR", serviceError: errors.New("private-storage-failure")},
		{name: "nil result", status: 500, calls: 1, code: "INTERNAL_ERROR"}, {name: "malformed result", status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "wrong parent", status: 500, calls: 1, code: "INTERNAL_ERROR"}, {name: "wrong name", status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "wrong size", status: 500, calls: 1, code: "INTERNAL_ERROR"}, {name: "wrong path", status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "cancel before entry", status: 500, code: "INTERNAL_ERROR"}, {name: "cancel before service", cancelOn: 2, status: 500, code: "INTERNAL_ERROR"},
		{name: "cancel after service", status: 500, calls: 1, code: "INTERNAL_ERROR"}, {name: "cancel with business error", status: 500, calls: 1, code: "INTERNAL_ERROR", serviceError: domain.ErrQuotaExceeded},
		{name: "cancel at delivery", cancelOn: 4, status: 500, calls: 1, code: "INTERNAL_ERROR"}, {name: "cancel before write", cancelOn: 5, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "service deadline", status: 500, calls: 1, code: "INTERNAL_ERROR"}, {name: "service panic", status: 500, calls: 1, code: "INTERNAL_ERROR"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			actor := domain.Actor{UserID: 7, DeviceID: 13, SessionID: 17}
			data := createUploadSuccessFixture(t)
			parentID, size, rawName, key := uint64(11), uint64(40), "学习资料 🌱", "Request-007"
			var rawSHA *string
			var expectedSHA *[32]byte
			switch fixture.name {
			case "trimmed name":
				rawName = "  学习资料 🌱\u3000"
			case "zero bytes":
				size, data.Task.SizeBytes = 0, 0
			case "single limit":
				size, data.Task.SizeBytes = domain.MaxSingleUploadSizeBytes, domain.MaxSingleUploadSizeBytes
			case "mixed SHA":
				value := strings.Repeat("aB", 32)
				rawSHA = &value
				expectedSHA = new([32]byte)
				for index := range expectedSHA {
					expectedSHA[index] = 0xab
				}
			case "maximum IDs":
				actor = domain.Actor{UserID: ^uint64(0), DeviceID: ^uint64(0), SessionID: ^uint64(0)}
				parentID = ^uint64(0)
				data.Task.ParentID = "18446744073709551615"
			case "oversize declaration":
				size = domain.MaxSingleUploadSizeBytes + 1
			case "maximum size declaration":
				size = ^uint64(0)
			case "wrong parent":
				data.Task.ParentID = "12"
			case "wrong name":
				data.Task.RequestedName = "private-wrong-name"
			case "wrong size":
				data.Task.SizeBytes = 41
			case "wrong path":
				data.ContentPath = "/private-wrong-path"
			}
			encodedRequest, err := json.Marshal(dto.CreateUploadRequest{ParentID: strconv.FormatUint(parentID, 10), Name: rawName, SizeBytes: &size, SHA256: rawSHA})
			if err != nil {
				t.Fatal(err)
			}
			requestBody := string(encodedRequest)
			if fixture.body != "" {
				requestBody = fixture.body
			}
			if fixture.name == "oversized JSON" {
				requestBody = strings.Repeat(" ", (1<<20)+1)
			}
			result, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			if fixture.name == "nil result" {
				result = nil
			}
			if fixture.name == "malformed result" {
				result = []byte(`{"task":null}`)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if fixture.name == "cancel before entry" {
				cancel()
			}
			if fixture.name == "service deadline" {
				var release context.CancelFunc
				ctx, release = context.WithTimeout(ctx, 20*time.Millisecond)
				defer release()
			}
			var logs bytes.Buffer
			var expectedContext context.Context
			calls, downstream := 0, 0
			router := NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
			create := handler.CreateUpload(func(received context.Context, current domain.Actor, input domain.CreateUploadInput, actualKey string) (json.RawMessage, error) {
				calls++
				if received != expectedContext || current != actor || input.ParentID != parentID || input.Name != rawName || input.SizeBytes != size || !reflect.DeepEqual(input.ExpectedSHA256, expectedSHA) || actualKey != key {
					t.Error("服务参数与可信身份、上传声明或幂等键不一致")
				}
				if fixture.name == "cancel after service" || fixture.name == "cancel with business error" {
					cancel()
				}
				if fixture.name == "service deadline" {
					<-received.Done()
					return nil, received.Err()
				}
				if fixture.name == "service panic" {
					panic("private-service-panic")
				}
				if fixture.serviceError != nil {
					return nil, fmt.Errorf("private-service-error: %w", fixture.serviceError)
				}
				return result, nil
			})
			router.POST("/test-create-upload", func(requestContext *gin.Context) {
				if fixture.name != "missing actor" {
					requestContext.Request = requestContext.Request.WithContext(requestcontext.WithActor(requestContext.Request.Context(), actor))
				}
				if fixture.cancelOn != 0 {
					requestContext.Request = requestContext.Request.WithContext(&cancelUserSessionsContext{Context: requestContext.Request.Context(), cancel: cancel, cancelOn: fixture.cancelOn})
				}
				expectedContext = requestContext.Request.Context()
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
			}, create, func(*gin.Context) { downstream++ })
			request := httptest.NewRequest("POST", "/test-create-upload?note=private-query", strings.NewReader(requestBody)).WithContext(ctx)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Origin", "http://127.0.0.1:5173")
			request.Header.Set("Idempotency-Key", " \t"+key+"\t ")
			request.Header.Set("Authorization", "Bearer private-access")
			request.AddCookie(&http.Cookie{Name: handler.RefreshCookieName, Value: "private-refresh"})
			switch fixture.name {
			case "missing key":
				request.Header.Del("Idempotency-Key")
			case "duplicate key":
				request.Header.Add("Idempotency-Key", "private-another")
			case "unsupported type":
				request.Header.Set("Content-Type", "text/plain")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != fixture.status || calls != fixture.calls || downstream != 0 || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" || response.Header().Get("Content-Type") != "application/json; charset=utf-8" || response.Header().Get("Content-Encoding") != "" || len(response.Header().Values("Set-Cookie")) != 0 {
				t.Fatal("处理顺序、状态或响应头错误", response.Code, calls)
			}
			requestID := response.Header().Get("X-Request-ID")
			if fixture.status == 201 {
				var body dto.CreateUploadResponse
				if json.Unmarshal(response.Body.Bytes(), &body) != nil || body.RequestID != requestID || !reflect.DeepEqual(body.Data, *data) {
					t.Fatal("上传创建成功快照或请求 ID 错误")
				}
			} else {
				var body dto.ErrorResponse
				if json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Error.Code != fixture.code || body.RequestID != requestID || body.Error.Message == "" {
					t.Fatal("上传创建错误结构错误", response.Body.String())
				}
				if fixture.code == "FILE_TOO_LARGE" {
					if len(body.Error.Details) != 2 || body.Error.Details["field"] != "size_bytes" || body.Error.Details["max_size_bytes"] != float64(domain.MaxSingleUploadSizeBytes) || body.Error.Message != domain.ErrUploadFileTooLarge.Error() {
						t.Fatal("文件超限字段或当前上限错误")
					}
				} else if fixture.field != "" {
					if len(body.Error.Details) != 1 || body.Error.Details["field"] != fixture.field {
						t.Fatal("错误字段定位错误")
					}
				} else if body.Error.Details != nil {
					t.Fatal("普通错误 details 应为 null")
				}
			}
			if (fixture.status == 401) != (response.Header().Get("WWW-Authenticate") != "") {
				t.Fatal("认证提示与状态不符")
			}
			var completed map[string]any
			if json.Unmarshal(logs.Bytes(), &completed) != nil || completed["request_id"] != requestID || completed["status"] != float64(fixture.status) || completed["path"] != "/test-create-upload" {
				t.Fatal("上传创建完成日志错误")
			}
			for _, marker := range []string{"private-query", "private-access", "private-refresh", "private-service", "private-storage", "private-wrong", "private-hash", "user_id", "Request-007"} {
				if strings.Contains(response.Body.String(), marker) || strings.Contains(logs.String(), marker) {
					t.Fatal("响应或日志包含原始请求及内部信息")
				}
			}
			if strings.Contains(logs.String(), "学习资料") {
				t.Fatal("完成日志包含业务名称")
			}
		})
	}
}

func TestCreateUploadHandlerAuthorization(t *testing.T) {
	for _, mode := range []string{"missing bearer", "cookie only", "query only", "invalid bearer", "preflight", "forbidden origin"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			router := NewRouter(logging.New(io.Discard), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
			router.POST("/test-create-upload", middleware.RequireAuthorization(func(context.Context, string) (*domain.Actor, error) { return nil, domain.ErrInvalidAccessToken }, handler.AuthorizationFailure), handler.CreateUpload(func(context.Context, domain.Actor, domain.CreateUploadInput, string) (json.RawMessage, error) {
				calls++
				return nil, nil
			}))
			request := httptest.NewRequest("POST", "/test-create-upload?access_token=private-query", strings.NewReader(`{"private-broken":`))
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

func TestCreateUploadHandlerCommittedResponse(t *testing.T) {
	for _, headersOnly := range []bool{false, true} {
		calls := 0
		router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
		router.POST("/test-create-upload", func(requestContext *gin.Context) {
			if headersOnly {
				requestContext.Status(202)
				requestContext.Writer.WriteHeaderNow()
			} else {
				requestContext.String(202, "already-written")
			}
		}, handler.CreateUpload(func(context.Context, domain.Actor, domain.CreateUploadInput, string) (json.RawMessage, error) {
			calls++
			return nil, nil
		}), func(*gin.Context) { t.Error("已发送响应进入下游") })
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest("POST", "/test-create-upload", nil))
		expected := "already-written"
		if headersOnly {
			expected = ""
		}
		if response.Code != 202 || response.Body.String() != expected || calls != 0 {
			t.Fatal("已发送响应被改写或调用服务")
		}
	}
}

func TestCreateUploadHandlerDependency(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("缺少服务仍装配成功")
		}
	}()
	handler.CreateUpload(nil)
}
