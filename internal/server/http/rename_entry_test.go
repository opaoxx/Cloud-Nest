package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

func TestRenameEntryHandlerFlow(t *testing.T) {
	for _, testCase := range []struct {
		name, body, code, field, current string
		status, calls, cancelOn          int
		serviceError                     error
	}{
		{name: "ordinary", status: 200, calls: 1}, {name: "trimmed name", status: 200, calls: 1}, {name: "same version", status: 200, calls: 1}, {name: "maximum integers", status: 200, calls: 1},
		{name: "missing actor", status: 500, code: "INTERNAL_ERROR"},
		{name: "invalid path before key", status: 400, code: "INVALID_ARGUMENT", field: "id", body: `{"private-broken":`},
		{name: "missing key before json", status: 400, code: "INVALID_ARGUMENT", field: "Idempotency-Key", body: `{"private-broken":`},
		{name: "duplicate key", status: 400, code: "INVALID_ARGUMENT", field: "Idempotency-Key"},
		{name: "invalid json", status: 400, code: "INVALID_ARGUMENT", body: `{"private-broken":`},
		{name: "unknown field", status: 400, code: "INVALID_ARGUMENT", body: `{"name":"学习笔记.PDF","expected_revision":"3","user_id":"99"}`},
		{name: "missing name", status: 400, code: "INVALID_ARGUMENT", field: "name", body: `{"expected_revision":"3"}`},
		{name: "zero version", status: 400, code: "INVALID_ARGUMENT", field: "expected_revision", body: `{"name":"学习笔记.PDF","expected_revision":"0"}`},
		{name: "numeric version", status: 400, code: "INVALID_ARGUMENT", body: `{"name":"学习笔记.PDF","expected_revision":3}`},
		{name: "unsupported type", status: 415, code: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "oversized body", status: 413, code: "REQUEST_BODY_TOO_LARGE"},
		{name: "invalid name", status: 400, calls: 1, code: "INVALID_NAME", field: "name", serviceError: domain.ErrInvalidEntryName},
		{name: "missing target", status: 404, calls: 1, code: "ENTRY_NOT_FOUND", serviceError: domain.ErrFileEntryNotFound},
		{name: "root", status: 403, calls: 1, code: "ROOT_OPERATION_FORBIDDEN", serviceError: domain.ErrRootEntryRename},
		{name: "revision conflict", status: 409, calls: 1, code: "REVISION_CONFLICT", current: "4", serviceError: &domain.EntryRevisionConflictError{CurrentRevision: 4}},
		{name: "maximum current revision", status: 409, calls: 1, code: "REVISION_CONFLICT", current: "18446744073709551615", serviceError: &domain.EntryRevisionConflictError{CurrentRevision: ^uint64(0)}},
		{name: "bare revision error", status: 500, calls: 1, code: "INTERNAL_ERROR", serviceError: domain.ErrEntryRevisionConflict},
		{name: "zero current revision", status: 500, calls: 1, code: "INTERNAL_ERROR", serviceError: &domain.EntryRevisionConflictError{}},
		{name: "nil typed conflict", status: 500, calls: 1, code: "INTERNAL_ERROR", serviceError: (*domain.EntryRevisionConflictError)(nil)},
		{name: "version limit", status: 500, calls: 1, code: "INTERNAL_ERROR", serviceError: domain.ErrEntryRevisionExhausted},
		{name: "name conflict", status: 409, calls: 1, code: "NAME_CONFLICT", serviceError: domain.ErrEntryNameConflict},
		{name: "key conflict", status: 409, calls: 1, code: "IDEMPOTENCY_CONFLICT", serviceError: domain.ErrIdempotencyRequestConflict},
		{name: "expired key", status: 409, calls: 1, code: "IDEMPOTENCY_EXPIRED", serviceError: domain.ErrIdempotencyRecordExpired},
		{name: "disabled", status: 403, calls: 1, code: "ACCOUNT_DISABLED", serviceError: domain.ErrAccountDisabled},
		{name: "revoked device", status: 401, calls: 1, code: "SESSION_REVOKED", serviceError: domain.ErrDeviceRevoked},
		{name: "revoked session", status: 401, calls: 1, code: "SESSION_REVOKED", serviceError: domain.ErrSessionRevoked},
		{name: "unknown fault", status: 500, calls: 1, code: "INTERNAL_ERROR", serviceError: errors.New("private-store-fault")},
		{name: "storage duplicate", status: 500, calls: 1, code: "INTERNAL_ERROR", serviceError: domain.ErrIdempotencyRecordAlreadyExists},
		{name: "nil result", status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "wrong target", status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "wrong name", status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "old result version", status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "future result version", status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "cancel before service", status: 500, code: "INTERNAL_ERROR", cancelOn: 2},
		{name: "cancel after service", status: 500, calls: 1, code: "INTERNAL_ERROR", cancelOn: 3},
		{name: "cancel before delivery", status: 500, calls: 1, code: "INTERNAL_ERROR", cancelOn: 5},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			data := renameEntrySuccessFixture(t)
			id, revision, name := "23", uint64(3), "学习笔记.PDF"
			switch testCase.name {
			case "trimmed name":
				name = "  学习笔记.PDF  "
			case "same version":
				data.Entry.Revision = "3"
			case "maximum integers":
				id = "18446744073709551615"
				revision = ^uint64(0)
				data.Entry.ID = id
				data.Entry.Revision = id
			case "invalid path before key":
				id = "0"
			case "wrong target":
				data.Entry.ID = "24"
			case "wrong name":
				data.Entry.Name = "private-wrong-name"
			case "old result version":
				data.Entry.Revision = "2"
			case "future result version":
				data.Entry.Revision = "6"
			}
			result, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			if testCase.name == "nil result" {
				result = nil
			}
			requestBody := testCase.body
			if requestBody == "" {
				encoded, err := json.Marshal(dto.RenameEntryRequest{Name: name, ExpectedRevision: strconv.FormatUint(revision, 10)})
				if err != nil {
					t.Fatal(err)
				}
				requestBody = string(encoded)
			}
			if testCase.name == "oversized body" {
				requestBody = strings.Repeat(" ", 1<<20) + requestBody
			}
			actor := domain.Actor{UserID: 7, DeviceID: 13, SessionID: 17}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls, downstream := 0, 0
			var expectedContext context.Context
			rename := handler.RenameEntry(func(ctx context.Context, current domain.Actor, target, expected uint64, rawName, key string) (json.RawMessage, error) {
				calls++
				if ctx != expectedContext || current != actor || strconv.FormatUint(target, 10) != id || expected != revision || rawName != name || key != "Request-007" {
					t.Error("服务参数或上下文错误")
				}
				if testCase.serviceError != nil {
					return result, fmt.Errorf("private-service: %w", testCase.serviceError)
				}
				return result, nil
			})
			var logs bytes.Buffer
			router := NewRouter(logging.New(&logs), nil, func(context.Context) bool { return true })
			router.PATCH("/test-rename-entry/:id", func(requestContext *gin.Context) {
				if testCase.name != "missing actor" {
					requestContext.Request = requestContext.Request.WithContext(requestcontext.WithActor(requestContext.Request.Context(), actor))
				}
				if testCase.cancelOn != 0 {
					requestContext.Request = requestContext.Request.WithContext(&cancelUserSessionsContext{Context: requestContext.Request.Context(), cancel: cancel, cancelOn: testCase.cancelOn})
				}
				expectedContext = requestContext.Request.Context()
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
			}, rename, func(*gin.Context) { downstream++ })
			request := httptest.NewRequest("PATCH", "/test-rename-entry/"+id+"?name=private-query", strings.NewReader(requestBody)).WithContext(ctx)
			request.Header.Set("Content-Type", "application/json")
			if testCase.name == "unsupported type" {
				request.Header.Set("Content-Type", "text/plain")
			}
			if testCase.name != "missing key before json" && testCase.name != "invalid path before key" {
				request.Header.Set("Idempotency-Key", " \tRequest-007\t ")
			}
			if testCase.name == "duplicate key" {
				request.Header.Add("Idempotency-Key", "Request-008")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != testCase.status || calls != testCase.calls || downstream != 0 || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" || response.Header().Get("Content-Type") != "application/json; charset=utf-8" || response.Header().Get("Content-Encoding") != "" {
				t.Fatal("状态、调用次数或响应头错误", response.Code, calls)
			}
			if testCase.status == 200 {
				var body dto.RenameEntryResponse
				if json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Data.Entry.Name != data.Entry.Name || body.Data.Entry.Revision != data.Entry.Revision || body.RequestID != response.Header().Get("X-Request-ID") {
					t.Fatal("成功快照或请求 ID 错误")
				}
			} else {
				var body dto.ErrorResponse
				if json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Error.Code != testCase.code || body.RequestID == "" || body.RequestID != response.Header().Get("X-Request-ID") {
					t.Fatal("错误响应错误", response.Body.String())
				}
				if testCase.field != "" {
					if len(body.Error.Details) != 1 || body.Error.Details["field"] != testCase.field {
						t.Fatal("错误字段错误")
					}
				} else if testCase.current != "" {
					if len(body.Error.Details) != 1 || body.Error.Details["current_revision"] != testCase.current {
						t.Fatal("当前版本错误")
					}
				} else if body.Error.Details != nil {
					t.Fatal("错误详情应为 null")
				}
				for _, marker := range []string{"private-service", "private-store-fault", "private-wrong-name", "private-query"} {
					if strings.Contains(response.Body.String(), marker) || strings.Contains(logs.String(), marker) {
						t.Fatal("响应或日志包含内部数据")
					}
				}
			}
		})
	}
}

func TestRenameEntryHandlerAuthorizationAndCommittedResponse(t *testing.T) {
	for _, mode := range []string{"missing authorization", "rejected authorization", "body written", "headers flushed"} {
		t.Run(mode, func(t *testing.T) {
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			rename := handler.RenameEntry(func(context.Context, domain.Actor, uint64, uint64, string, string) (json.RawMessage, error) {
				t.Error("入口拒绝后调用服务")
				return nil, nil
			})
			if strings.Contains(mode, "authorization") {
				router.PATCH("/test-rename-entry/:id", middleware.RequireAuthorization(func(context.Context, string) (*domain.Actor, error) { return nil, domain.ErrInvalidAccessToken }, handler.AuthorizationFailure), rename)
			} else {
				router.PATCH("/test-rename-entry/:id", func(ctx *gin.Context) {
					if mode == "body written" {
						ctx.String(202, "already-written")
					} else {
						ctx.Status(202)
						ctx.Writer.WriteHeaderNow()
					}
				}, rename, func(*gin.Context) { t.Error("已提交响应进入下游") })
			}
			request := httptest.NewRequest("PATCH", "/test-rename-entry/0", strings.NewReader(`{"private-broken":`))
			if mode == "rejected authorization" {
				request.Header.Set("Authorization", "Bearer private-access")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if strings.Contains(mode, "authorization") {
				if response.Code != 401 || response.Header().Get("WWW-Authenticate") == "" {
					t.Fatal("认证未先于参数校验")
				}
			} else if response.Code != 202 || (mode == "body written" && response.Body.String() != "already-written") || (mode == "headers flushed" && response.Body.Len() != 0) {
				t.Fatal("已提交响应被改写")
			}
		})
	}
}

func TestRenameEntryHandlerDependency(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("缺少依赖未拒绝")
		}
	}()
	handler.RenameEntry(nil)
}
