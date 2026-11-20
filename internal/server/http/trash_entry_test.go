package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
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

func TestTrashEntryHandlerFlow(t *testing.T) {
	const basePath = "/test-trash-entry/"
	const defaultBody = `{"expected_revision":"3"}`
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	for _, scenario := range []struct {
		name, path, body, code, field, resultMode string
		entryID, revision                         uint64
		status, calls, cancelOn                   int
		noBearer, noKey, duplicateKey             bool
		unsupportedType, oversized                bool
		serviceError                              error
	}{
		{name: "file", status: 200, calls: 1},
		{name: "directory with maximum IDs and new revision", entryID: math.MaxUint64, revision: math.MaxUint64 - 1,
			body: `{"expected_revision":"18446744073709551614"}`, resultMode: "directory", status: 200, calls: 1},
		{name: "authentication first", path: "0", body: `{"private-malformed":`, noBearer: true, noKey: true, status: 401, code: "AUTH_REQUIRED"},
		{name: "path before key and body", path: "0", noKey: true, body: `{"private-malformed":`, status: 400, code: "INVALID_ARGUMENT", field: "id"},
		{name: "key before body", noKey: true, body: `{"private-malformed":`, status: 400, code: "INVALID_ARGUMENT", field: "Idempotency-Key"},
		{name: "duplicate keys", duplicateKey: true, status: 400, code: "INVALID_ARGUMENT", field: "Idempotency-Key"},
		{name: "wrong media type", unsupportedType: true, status: 415, code: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "oversized body", oversized: true, status: 413, code: "REQUEST_BODY_TOO_LARGE"},
		{name: "unknown identity field", body: `{"expected_revision":"3","user_id":"99"}`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "numeric revision", body: `{"expected_revision":3}`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "missing revision", body: `{}`, status: 400, code: "INVALID_ARGUMENT", field: "expected_revision"},
		{name: "revision overflow", body: `{"expected_revision":"18446744073709551616"}`, status: 400, code: "INVALID_ARGUMENT", field: "expected_revision"},
		{name: "root forbidden", serviceError: fmt.Errorf("private-trace: %w", domain.ErrRootEntryTrash), status: 403, calls: 1, code: "ROOT_OPERATION_FORBIDDEN"},
		{name: "invisible entry", serviceError: domain.ErrFileEntryNotFound, status: 404, calls: 1, code: "ENTRY_NOT_FOUND"},
		{name: "current revision", serviceError: fmt.Errorf("private-trace: %w", &domain.EntryRevisionConflictError{CurrentRevision: math.MaxUint64}),
			status: 409, calls: 1, code: "REVISION_CONFLICT"},
		{name: "zero current revision", serviceError: &domain.EntryRevisionConflictError{}, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "missing current revision", serviceError: domain.ErrEntryRevisionConflict, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "idempotency conflict", serviceError: domain.ErrIdempotencyRequestConflict, status: 409, calls: 1, code: "IDEMPOTENCY_CONFLICT"},
		{name: "expired record", serviceError: domain.ErrIdempotencyRecordExpired, status: 409, calls: 1, code: "IDEMPOTENCY_EXPIRED"},
		{name: "disabled account", serviceError: domain.ErrAccountDisabled, status: 403, calls: 1, code: "ACCOUNT_DISABLED"},
		{name: "revoked session", serviceError: domain.ErrSessionRevoked, status: 401, calls: 1, code: "SESSION_REVOKED"},
		{name: "exhausted revision", serviceError: domain.ErrEntryRevisionExhausted, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "invalid stored data", serviceError: domain.ErrInvalidIdempotencyRecordData, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "unknown error", serviceError: errors.New("private-trace"), status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "missing success result", resultMode: "nil", status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "wrong successful ID", resultMode: "wrong ID", status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "wrong successful revision", resultMode: "unchanged", status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "success with exhausted request revision", revision: math.MaxUint64, body: `{"expected_revision":"18446744073709551615"}`,
			resultMode: "unchanged", status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "cancel before service", cancelOn: 2, status: 500, code: "INTERNAL_ERROR"},
		{name: "cancel after service", cancelOn: 3, status: 500, calls: 1, code: "INTERNAL_ERROR", serviceError: domain.ErrFileEntryNotFound},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 通过共享授权和 Handler 校验请求边界，用替身核对服务参数。
			actor := domain.Actor{UserID: 7, DeviceID: 19, SessionID: 17}
			entryID, revision := scenario.entryID, scenario.revision
			if entryID == 0 {
				entryID = 23
			}
			if revision == 0 {
				revision = 3
			}
			data, err := dto.RestoreTrashEntryData([]byte(trashSuccessTestBody))
			if err != nil {
				t.Fatal(err)
			}
			data.Entry.ID = strconv.FormatUint(entryID, 10)
			data.Entry.Revision = strconv.FormatUint(revision+1, 10)
			switch scenario.resultMode {
			case "directory":
				data.Entry.EntryType = "DIR"
			case "wrong ID":
				data.Entry.ID = "24"
			case "unchanged":
				data.Entry.Revision = strconv.FormatUint(revision, 10)
			}
			stableBody, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			if scenario.resultMode == "nil" {
				stableBody = nil
			}

			calls := 0
			var activeContext context.Context
			handle := handler.TrashEntry(func(ctx context.Context, identity domain.Actor, targetID, expectedRevision uint64, key string) (json.RawMessage, error) {
				calls++
				if ctx != activeContext || identity != actor || targetID != entryID || expectedRevision != revision ||
					key != "Trash-Key" || requestcontext.RequestID(ctx) != requestID {
					t.Error("幂等回收服务未取得当前身份和完整请求参数")
				}
				return stableBody, scenario.serviceError
			})
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			auth := middleware.RequireAuthorization(func(context.Context, string) (*domain.Actor, error) { return &actor, nil }, handler.AuthorizationFailure)
			router.POST(basePath+":id", auth, func(requestContext *gin.Context) {
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
				activeContext = requestContext.Request.Context()
				if scenario.cancelOn != 0 {
					ctx, cancel := context.WithCancel(activeContext)
					defer cancel()
					activeContext = &cancelUserSessionsContext{Context: ctx, cancel: cancel, cancelOn: scenario.cancelOn}
					requestContext.Request = requestContext.Request.WithContext(activeContext)
				}
				handle(requestContext)
			}, func(*gin.Context) { t.Error("回收响应发出后继续执行") })

			// 2. 发送测试请求，核对错误定位、状态和禁止缓存响应头。
			path, body := scenario.path, scenario.body
			if path == "" {
				path = strconv.FormatUint(entryID, 10)
			}
			if body == "" {
				body = defaultBody
			}
			if scenario.oversized {
				body = strings.Repeat(" ", int(binding.MaxJSONBodyBytes)) + body
			}
			request := httptest.NewRequest(http.MethodPost, basePath+path+"?user_id=99", strings.NewReader(body))
			request.Header.Set("X-Request-ID", requestID)
			request.Header.Set("X-User-ID", "99")
			request.Header.Set("Content-Type", "application/json")
			if scenario.unsupportedType {
				request.Header.Set("Content-Type", "text/plain")
			}
			if !scenario.noBearer {
				request.Header.Set("Authorization", "Bearer private-credential")
			}
			if !scenario.noKey {
				request.Header.Add("Idempotency-Key", " \tTrash-Key\t ")
			}
			if scenario.duplicateKey {
				request.Header.Add("Idempotency-Key", "Another-Key")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			header := response.Header()
			if response.Code != scenario.status || calls != scenario.calls ||
				header.Get("Content-Type") != "application/json; charset=utf-8" || header.Get("Content-Encoding") != "" ||
				header.Get("Cache-Control") != "no-store" || header.Get("X-Request-ID") != requestID ||
				(!scenario.noBearer && header.Get("Pragma") != "no-cache") ||
				(header.Get("WWW-Authenticate") != "") != (scenario.status == http.StatusUnauthorized) {
				t.Fatal("回收响应状态、调用次数或响应头不符", response.Code, calls)
			}
			if scenario.status == http.StatusOK {
				var success dto.TrashEntryResponse
				if json.Unmarshal(response.Body.Bytes(), &success) != nil || success.RequestID != requestID || !reflect.DeepEqual(success.Data, *data) {
					t.Fatal("回收成功快照与请求不符")
				}
				return
			}
			var details map[string]any
			if scenario.field != "" {
				details = map[string]any{"field": scenario.field}
			} else if scenario.code == "REVISION_CONFLICT" {
				details = map[string]any{"current_revision": "18446744073709551615"}
			}
			var failure dto.ErrorResponse
			if json.Unmarshal(response.Body.Bytes(), &failure) != nil || failure.RequestID != requestID ||
				failure.Error.Code != scenario.code || !reflect.DeepEqual(failure.Error.Details, details) ||
				strings.Contains(response.Body.String(), "private-") || strings.Contains(response.Body.String(), `"data"`) {
				t.Fatal("回收错误正文不符或包含内部信息", failure.Error.Code)
			}
		})
	}
}

func TestTrashEntryHandlerStopsBeforeService(t *testing.T) {
	for _, mode := range []string{"missing actor", "written", "aborted"} {
		t.Run(mode, func(t *testing.T) {
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			handle := handler.TrashEntry(func(context.Context, domain.Actor, uint64, uint64, string) (json.RawMessage, error) {
				t.Error("缺少身份或已结束的请求进入了服务")
				return nil, nil
			})
			wantStatus, wantBody := 409, ""
			router.POST("/test-trash/:id", func(requestContext *gin.Context) {
				switch mode {
				case "missing actor":
					wantStatus = 500
				case "written":
					wantBody = "existing result"
					requestContext.String(wantStatus, wantBody)
				case "aborted":
					requestContext.Status(wantStatus)
					requestContext.Abort()
				}
				handle(requestContext)
			})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/test-trash/23", nil))
			if response.Code != wantStatus || (mode != "missing actor" && response.Body.String() != wantBody) {
				t.Fatal("提前结束状态不符", response.Code)
			}
		})
	}
}
