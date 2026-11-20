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

func TestRestoreEntryHandlerFlow(t *testing.T) {
	const path = "/test-trash/"
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	for _, scenario := range []struct {
		name, rawID, body, code, field, resultMode string
		entryID, revision                          uint64
		status, calls, cancelOn                    int
		noBearer, noKey, duplicateKey              bool
		wrongMedia, oversized                      bool
		serviceError                               error
	}{
		{name: "file", status: 200, calls: 1},
		{name: "directory with highest ID and result version", entryID: math.MaxUint64, revision: math.MaxUint64 - 1, resultMode: "directory", status: 200, calls: 1},
		{name: "authentication first", rawID: "0", body: `{`, noBearer: true, noKey: true, status: 401, code: "AUTH_REQUIRED"},
		{name: "path before key and body", rawID: "0", body: `{`, noKey: true, status: 400, code: "INVALID_ARGUMENT", field: "id"},
		{name: "key before body", body: `{`, noKey: true, status: 400, code: "INVALID_ARGUMENT", field: "Idempotency-Key"},
		{name: "duplicate key", duplicateKey: true, status: 400, code: "INVALID_ARGUMENT", field: "Idempotency-Key"},
		{name: "invalid version type", body: `{"expected_revision":4}`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "missing version", body: `{}`, status: 400, code: "INVALID_ARGUMENT", field: "expected_revision"},
		{name: "version overflow", body: `{"expected_revision":"18446744073709551616"}`, status: 400, code: "INVALID_ARGUMENT", field: "expected_revision"},
		{name: "unknown field", body: `{"expected_revision":"4","user_id":"99"}`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "size limit", oversized: true, status: 413, code: "REQUEST_BODY_TOO_LARGE"},
		{name: "media type", wrongMedia: true, status: 415, code: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "root forbidden", serviceError: fmt.Errorf("private-detail: %w", domain.ErrRootEntryRestore), status: 403, calls: 1, code: "ROOT_OPERATION_FORBIDDEN"},
		{name: "unavailable entry", serviceError: domain.ErrFileEntryNotFound, status: 404, calls: 1, code: "ENTRY_NOT_FOUND"},
		{name: "unavailable parent", serviceError: fmt.Errorf("private-detail: %w", domain.ErrRestoreTargetUnavailable), status: 409, calls: 1, code: "TARGET_UNAVAILABLE"},
		{name: "name conflict", serviceError: fmt.Errorf("private-detail: %w", domain.ErrEntryNameConflict), status: 409, calls: 1, code: "NAME_CONFLICT"},
		{name: "revision conflict", serviceError: &domain.EntryRevisionConflictError{CurrentRevision: math.MaxUint64}, status: 409, calls: 1, code: "REVISION_CONFLICT"},
		{name: "revision conflict missing current version", serviceError: &domain.EntryRevisionConflictError{}, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "reused key with other input", serviceError: domain.ErrIdempotencyRequestConflict, status: 409, calls: 1, code: "IDEMPOTENCY_CONFLICT"},
		{name: "expired key", serviceError: domain.ErrIdempotencyRecordExpired, status: 409, calls: 1, code: "IDEMPOTENCY_EXPIRED"},
		{name: "revoked in transaction", serviceError: domain.ErrSessionRevoked, status: 401, calls: 1, code: "SESSION_REVOKED"},
		{name: "disabled in transaction", serviceError: domain.ErrAccountDisabled, status: 403, calls: 1, code: "ACCOUNT_DISABLED"},
		{name: "exhausted version", serviceError: domain.ErrEntryRevisionExhausted, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "unknown internal error", serviceError: errors.New("private-detail"), status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "missing success body", resultMode: "nil", status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "wrong result target", resultMode: "wrong ID", status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "unchanged result version", resultMode: "unchanged", status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "unsafe result increment", revision: math.MaxUint64, resultMode: "unchanged", status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "cancel before service", cancelOn: 2, status: 500, code: "INTERNAL_ERROR"},
		{name: "cancel after service", cancelOn: 3, status: 500, calls: 1, code: "INTERNAL_ERROR"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			entryID, revision := scenario.entryID, scenario.revision
			if entryID == 0 {
				entryID = 23
			}
			if revision == 0 {
				revision = 4
			}
			data, err := dto.DecodeRestoreEntryData([]byte(restoreSuccessTestBody))
			if err != nil {
				t.Fatal(err)
			}
			data.Entry.ID = strconv.FormatUint(entryID, 10)
			if revision < math.MaxUint64 {
				data.Entry.Revision = strconv.FormatUint(revision+1, 10)
			}
			switch scenario.resultMode {
			case "directory":
				data.Entry.EntryType = "DIR"
			case "wrong ID":
				data.Entry.ID = "24"
			case "unchanged":
				data.Entry.Revision = strconv.FormatUint(revision, 10)
			}
			result, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			if scenario.resultMode == "nil" {
				result = nil
			}

			// 1. 经共享认证进入处理函数，核对服务实际收到的参数。
			actor := domain.Actor{UserID: 7, DeviceID: 19, SessionID: 17}
			calls := 0
			restore := handler.RestoreEntry(func(ctx context.Context, identity domain.Actor, targetID, expectedRevision uint64, key string) (json.RawMessage, error) {
				calls++
				if identity != actor || targetID != entryID || expectedRevision != revision ||
					key != "Restore-Key" || requestcontext.RequestID(ctx) != requestID {
					t.Fatal("恢复服务收到错误的身份、目标、版本或幂等键")
				}
				return result, scenario.serviceError
			})
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			auth := middleware.RequireAuthorization(func(context.Context, string) (*domain.Actor, error) { return &actor, nil }, handler.AuthorizationFailure)
			router.POST(path+":id/restore", auth, func(requestContext *gin.Context) {
				requestContext.Header("Content-Encoding", "gzip")
				requestContext.Header("Content-Length", "3")
				if scenario.cancelOn != 0 {
					ctx, cancel := context.WithCancel(requestContext.Request.Context())
					defer cancel()
					requestContext.Request = requestContext.Request.WithContext(&cancelUserSessionsContext{Context: ctx, cancel: cancel, cancelOn: scenario.cancelOn})
				}
				restore(requestContext)
			}, func(*gin.Context) { t.Error("恢复响应发出后继续执行了下游") })

			rawID, body := scenario.rawID, scenario.body
			if rawID == "" {
				rawID = strconv.FormatUint(entryID, 10)
			}
			if body == "" {
				body = fmt.Sprintf(`{"expected_revision":"%d"}`, revision)
			}
			if scenario.oversized {
				body = strings.Repeat(" ", int(binding.MaxJSONBodyBytes)) + body
			}
			request := httptest.NewRequest(http.MethodPost, path+rawID+"/restore?user_id=99", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			if scenario.wrongMedia {
				request.Header.Set("Content-Type", "text/plain")
			}
			request.Header.Set("X-Request-ID", requestID)
			if !scenario.noBearer {
				request.Header.Set("Authorization", "Bearer private-credential")
			}
			if !scenario.noKey {
				request.Header.Add("Idempotency-Key", " \tRestore-Key\t ")
			}
			if scenario.duplicateKey {
				request.Header.Add("Idempotency-Key", "Other-Key")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			// 2. 校验成功或失败以及响应头，保证错误只交付公开信息。
			header := response.Header()
			contentLength := header.Get("Content-Length")
			if response.Code != scenario.status || calls != scenario.calls || header.Get("X-Request-ID") != requestID ||
				header.Get("Content-Type") != "application/json; charset=utf-8" || header.Get("Content-Encoding") != "" ||
				(contentLength != "" && contentLength != strconv.Itoa(response.Body.Len())) || header.Get("Cache-Control") != "no-store" ||
				(!scenario.noBearer && header.Get("Pragma") != "no-cache") ||
				(header.Get("WWW-Authenticate") != "") != (scenario.status == http.StatusUnauthorized) {
				t.Fatalf("响应或调用次数不符，状态 %d，服务调用 %d，响应头 %v", response.Code, calls, header)
			}
			if scenario.status == http.StatusOK {
				var success dto.RestoreEntryResponse
				if err := json.Unmarshal(response.Body.Bytes(), &success); err != nil || success.RequestID != requestID || success.Data != *data {
					t.Fatal("成功响应与快照不符", err)
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
			if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil || failure.RequestID != requestID ||
				failure.Error.Code != scenario.code || !reflect.DeepEqual(failure.Error.Details, details) ||
				strings.Contains(response.Body.String(), "private-") || strings.Contains(response.Body.String(), `"data"`) {
				t.Fatal("恢复错误不符合契约或泄漏内部信息", failure, err)
			}
		})
	}
}
