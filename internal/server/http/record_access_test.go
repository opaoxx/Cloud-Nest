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
	"time"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

func TestRecordAccessHandlerFlow(t *testing.T) {
	for _, testCase := range []struct {
		name, body, path, purpose, code, field string
		status, calls, cancelOn                int
		serviceError                           error
	}{
		{name: "preview", status: 200, calls: 1},
		{name: "open", body: `{"purpose":"open"}`, purpose: "open", status: 200, calls: 1},
		{name: "maximum target", path: "18446744073709551615", status: 200, calls: 1},
		{name: "maximum actor", status: 200, calls: 1},
		{name: "encoded target", path: "%31%33", status: 200, calls: 1},
		{name: "extra key header", status: 200, calls: 1},
		{name: "duplicate last open", body: `{"purpose":"preview","purpose":"open"}`, purpose: "open", status: 200, calls: 1},
		{name: "pending success headers", status: 200, calls: 1},
		{name: "missing actor", path: "0", body: `{"private-broken":`, status: 500, code: "INTERNAL_ERROR"},
		{name: "invalid path before body", path: "0", body: `{"private-broken":`, status: 400, code: "INVALID_ARGUMENT", field: "id"},
		{name: "leading zero", path: "013", status: 400, code: "INVALID_ARGUMENT", field: "id"},
		{name: "overflow", path: "18446744073709551616", status: 400, code: "INVALID_ARGUMENT", field: "id"},
		{name: "missing purpose", body: `{}`, status: 400, code: "INVALID_ARGUMENT", field: "purpose"},
		{name: "null purpose", body: `{"purpose":null}`, status: 400, code: "INVALID_ARGUMENT", field: "purpose"},
		{name: "uppercase purpose", body: `{"purpose":"PREVIEW"}`, status: 400, code: "INVALID_ARGUMENT", field: "purpose"},
		{name: "padded purpose", body: `{"purpose":" open "}`, status: 400, code: "INVALID_ARGUMENT", field: "purpose"},
		{name: "download purpose", body: `{"purpose":"download"}`, status: 400, code: "INVALID_ARGUMENT", field: "purpose"},
		{name: "boolean purpose", body: `{"purpose":true}`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "unknown owner", body: `{"purpose":"open","user_id":"99"}`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "client time", body: `{"purpose":"open","last_opened_at":"2026-11-16T00:00:00Z"}`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "revision field", body: `{"purpose":"open","expected_revision":"3"}`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "null body", body: `null`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "malformed body", body: `{"private-broken":`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "unsupported type", status: 415, code: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "compressed body", status: 415, code: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "oversized body", status: 413, code: "REQUEST_BODY_TOO_LARGE"},
		{name: "missing or hidden target", status: 404, calls: 1, code: "ENTRY_NOT_FOUND", serviceError: domain.ErrFileEntryNotFound},
		{name: "wrapped missing target", status: 404, calls: 1, code: "ENTRY_NOT_FOUND", serviceError: fmt.Errorf("private-store-fault: %w", domain.ErrFileEntryNotFound)},
		{name: "pending error headers", status: 404, calls: 1, code: "ENTRY_NOT_FOUND", serviceError: domain.ErrFileEntryNotFound},
		{name: "directory target", status: 400, calls: 1, code: "INVALID_ARGUMENT", field: "id", serviceError: domain.ErrFileEntryNotFile},
		{name: "root target", path: "11", status: 400, calls: 1, code: "INVALID_ARGUMENT", field: "id", serviceError: domain.ErrFileEntryNotFile},
		{name: "wrapped file type", status: 400, calls: 1, code: "INVALID_ARGUMENT", field: "id", serviceError: fmt.Errorf("private-store-fault: %w", domain.ErrFileEntryNotFile)},
		{name: "service purpose", status: 400, calls: 1, code: "INVALID_ARGUMENT", field: "purpose", serviceError: domain.ErrInvalidEntryAccessPurpose},
		{name: "wrapped service purpose", status: 400, calls: 1, code: "INVALID_ARGUMENT", field: "purpose", serviceError: fmt.Errorf("private-store-fault: %w", domain.ErrInvalidEntryAccessPurpose)},
		{name: "missing account", status: 401, calls: 1, code: "AUTH_REQUIRED", serviceError: domain.ErrAccountNotFound},
		{name: "disabled account", status: 403, calls: 1, code: "ACCOUNT_DISABLED", serviceError: domain.ErrAccountDisabled},
		{name: "missing session", status: 401, calls: 1, code: "SESSION_REVOKED", serviceError: domain.ErrSessionNotFound},
		{name: "revoked session", status: 401, calls: 1, code: "SESSION_REVOKED", serviceError: domain.ErrSessionRevoked},
		{name: "expired session", status: 401, calls: 1, code: "SESSION_REVOKED", serviceError: domain.ErrSessionExpired},
		{name: "revoked device", status: 401, calls: 1, code: "SESSION_REVOKED", serviceError: domain.ErrDeviceRevoked},
		{name: "invalid stored data", status: 500, calls: 1, code: "INTERNAL_ERROR", serviceError: domain.ErrInvalidFileEntryData},
		{name: "unknown fault", status: 500, calls: 1, code: "INTERNAL_ERROR", serviceError: errors.New("private-store-fault")},
		{name: "nil result", status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "wrong owner", status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "wrong target", status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "missing access result", status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "invalid result time", status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "canceled entrance", status: 500, code: "INTERNAL_ERROR"},
		{name: "canceled before service", cancelOn: 2, status: 500, code: "INTERNAL_ERROR"},
		{name: "canceled after service", cancelOn: 3, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "canceled before target error", cancelOn: 3, status: 500, calls: 1, code: "INTERNAL_ERROR", serviceError: domain.ErrFileEntryNotFound},
		{name: "canceled at delivery", cancelOn: 4, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "canceled before write", cancelOn: 5, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "deadline", status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "service panic", status: 500, calls: 1, code: "INTERNAL_ERROR"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 注入当前身份和服务，核对上下文、目标及用途。
			actor := domain.Actor{UserID: 7, DeviceID: 19, SessionID: 17}
			if testCase.name == "maximum actor" {
				actor = domain.Actor{UserID: ^uint64(0), DeviceID: ^uint64(0), SessionID: ^uint64(0)}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if testCase.name == "canceled entrance" {
				cancel()
			}
			if testCase.name == "deadline" {
				var release context.CancelFunc
				ctx, release = context.WithTimeout(ctx, 20*time.Millisecond)
				defer release()
			}
			targetID := uint64(13)
			if testCase.name == "maximum target" {
				targetID = ^uint64(0)
			}
			if testCase.name == "root target" {
				targetID = 11
			}
			expectedPurpose := testCase.purpose
			if expectedPurpose == "" {
				expectedPurpose = "preview"
			}
			var expectedContext context.Context
			calls := 0
			recordAccess := handler.RecordAccess(func(received context.Context, identity domain.Actor, entryID uint64, purpose string) (*domain.EntryPreference, error) {
				calls++
				if received != expectedContext || identity != actor || entryID != targetID || purpose != expectedPurpose {
					t.Error("服务收到的上下文、身份、目标或用途错误")
				}
				if testCase.name == "deadline" {
					<-received.Done()
					return nil, received.Err()
				}
				if testCase.name == "service panic" {
					panic("private-service-panic")
				}
				if testCase.name == "nil result" {
					return nil, nil
				}
				result := recordAccessSuccessFixture()
				result.UserID, result.EntryID = identity.UserID, entryID
				switch testCase.name {
				case "wrong owner":
					result.UserID++
				case "wrong target":
					result.EntryID++
				case "missing access result":
					result.LastOpenedAt = nil
				case "invalid result time":
					*result.LastOpenedAt = time.Time{}
				}
				return result, testCase.serviceError
			})
			var logs bytes.Buffer
			router := NewRouter(logging.New(&logs), nil, func(context.Context) bool { return true })
			router.POST("/test-record-access/:id", func(requestContext *gin.Context) {
				if testCase.name != "missing actor" {
					requestContext.Request = requestContext.Request.WithContext(requestcontext.WithActor(requestContext.Request.Context(), actor))
				}
				if testCase.cancelOn != 0 {
					requestContext.Request = requestContext.Request.WithContext(&cancelUserSessionsContext{Context: requestContext.Request.Context(), cancel: cancel, cancelOn: testCase.cancelOn})
				}
				if strings.HasPrefix(testCase.name, "pending ") {
					requestContext.Header("Content-Length", "3")
					requestContext.Header("Content-Encoding", "gzip")
					requestContext.Header("Content-Type", "application/octet-stream")
				}
				expectedContext = requestContext.Request.Context()
			}, recordAccess, func(*gin.Context) { t.Error("处理完成后进入下游") })
			path, body := testCase.path, testCase.body
			if path == "" {
				path = "13"
			}
			if body == "" {
				body = `{"purpose":"preview"}`
			}
			if testCase.name == "oversized body" {
				body = strings.Repeat(" ", (1<<20)+1) + body
			}
			request := httptest.NewRequest("POST", "/test-record-access/"+path+"?user_id=99&id=99&purpose=private-query", strings.NewReader(body)).WithContext(ctx)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Request-ID", "28EE758A-0727-42DE-AE7D-3BB33155E148")
			request.Header.Set("X-User-ID", "99")
			if testCase.name == "unsupported type" {
				request.Header.Set("Content-Type", "text/plain")
			}
			if testCase.name == "compressed body" {
				request.Header.Set("Content-Encoding", "gzip")
			}
			if testCase.name == "extra key header" {
				request.Header.Add("Idempotency-Key", "private-key")
				request.Header.Add("Idempotency-Key", "private-key-2")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			// 2. 核对状态、服务次数、错误字段、缓存和请求关联。
			if response.Code != testCase.status || calls != testCase.calls || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" || response.Header().Get("Content-Type") != "application/json; charset=utf-8" || response.Header().Get("Content-Encoding") != "" || response.Header().Get("Set-Cookie") != "" || response.Header().Get("X-Request-ID") != "28ee758a-0727-42de-ae7d-3bb33155e148" {
				t.Fatal("状态、服务次数或响应头错误", response.Code, calls)
			}
			if testCase.status == 200 {
				var result dto.RecordAccessResponse
				if json.Unmarshal(response.Body.Bytes(), &result) != nil || result.RequestID != response.Header().Get("X-Request-ID") || !result.Data.LastOpenedAt.Equal(recordAccessSuccessFixture().LastOpenedAt.UTC().Truncate(time.Microsecond)) || response.Header().Get("Content-Length") != strconv.Itoa(response.Body.Len()) {
					t.Fatal("成功访问或正文头错误")
				}
			} else {
				var result dto.ErrorResponse
				if json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Error.Code != testCase.code || result.RequestID != response.Header().Get("X-Request-ID") {
					t.Fatal("错误响应不符", response.Body.String())
				}
				if testCase.field != "" {
					if len(result.Error.Details) != 1 || result.Error.Details["field"] != testCase.field {
						t.Fatal("错误字段位置不符")
					}
				} else if result.Error.Details != nil {
					t.Fatal("错误详情须为 null")
				}
				if testCase.status == 404 && result.Error.Message != domain.ErrFileEntryNotFound.Error() {
					t.Fatal("不可见目标提示错误")
				}
			}
			if (testCase.status == 401) != (response.Header().Get("WWW-Authenticate") != "") {
				t.Fatal("认证提示头错误")
			}
			var completed map[string]any
			if json.Unmarshal(logs.Bytes(), &completed) != nil || completed["request_id"] != response.Header().Get("X-Request-ID") || completed["status"] != float64(testCase.status) {
				t.Fatal("完成日志错误")
			}
			for _, marker := range []string{"private-query", "private-store-fault", "private-service-panic", "private-key", "private-broken"} {
				if strings.Contains(response.Body.String(), marker) || strings.Contains(logs.String(), marker) {
					t.Fatal("响应或日志包含内部/原始请求值")
				}
			}
		})
	}
}

func TestRecordAccessHandlerAuthorizationAndCommittedResponse(t *testing.T) {
	for _, mode := range []string{"missing authorization", "rejected authorization", "body written", "headers flushed"} {
		t.Run(mode, func(t *testing.T) {
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			recordAccess := handler.RecordAccess(func(context.Context, domain.Actor, uint64, string) (*domain.EntryPreference, error) {
				t.Error("入口拒绝后调用服务")
				return nil, nil
			})
			if strings.Contains(mode, "authorization") {
				router.POST("/test-record-access/:id", middleware.RequireAuthorization(func(context.Context, string) (*domain.Actor, error) { return nil, domain.ErrInvalidAccessToken }, handler.AuthorizationFailure), recordAccess)
			} else {
				router.POST("/test-record-access/:id", func(ctx *gin.Context) {
					if mode == "body written" {
						ctx.String(202, "already-written")
					} else {
						ctx.Status(202)
						ctx.Writer.WriteHeaderNow()
					}
				}, recordAccess, func(*gin.Context) { t.Error("已发送响应进入下游") })
			}
			request := httptest.NewRequest("POST", "/test-record-access/0", strings.NewReader(`{"private-broken":`))
			if mode == "rejected authorization" {
				request.Header.Set("Authorization", "Bearer private-access")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if strings.Contains(mode, "authorization") {
				var result dto.ErrorResponse
				if response.Code != 401 || response.Header().Get("WWW-Authenticate") == "" || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Error.Code != "AUTH_REQUIRED" || result.Error.Details != nil {
					t.Fatal("认证未先于参数校验")
				}
			} else if response.Code != 202 || (mode == "body written" && response.Body.String() != "already-written") || (mode == "headers flushed" && response.Body.Len() != 0) || response.Header().Get("Cache-Control") != "" {
				t.Fatal("已发送响应被改写")
			}
		})
	}
}

func TestRecordAccessHandlerDependency(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("缺少服务依赖未拒绝")
		}
	}()
	handler.RecordAccess(nil)
}
