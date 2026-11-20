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
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

func TestCreateDownloadHandlerFlow(t *testing.T) {
	const defaultBody = `{"expected_revision":"3","purpose":"download"}`
	const route = "/test-download/:id"
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	for _, scenario := range []struct {
		name, path, body, purpose, code, field string
		entryID, revision                      uint64
		status, calls, cancelOn                int
		noBearer, invalidType, oversized       bool
		nilResult, wrongRevision               bool
		serviceError                           error
	}{
		{name: "download", status: 200, calls: 1},
		{name: "preview maximum values", path: "18446744073709551615",
			body: `{"expected_revision":"18446744073709551615","purpose":"preview"}`, purpose: "preview",
			entryID: math.MaxUint64, revision: math.MaxUint64, status: 200, calls: 1},
		{name: "authentication before body", path: "0", body: `{"private-malformed":`, noBearer: true, status: 401, code: "AUTH_REQUIRED"},
		{name: "path before body", path: "0", body: `{"private-malformed":`, status: 400, code: "INVALID_ARGUMENT", field: "id"},
		{name: "body type", invalidType: true, status: 415, code: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "body size", oversized: true, status: 413, code: "REQUEST_BODY_TOO_LARGE"},
		{name: "non object", body: "null", status: 400, code: "INVALID_ARGUMENT"},
		{name: "body user override", body: `{"expected_revision":"3","purpose":"download","user_id":"99"}`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "revision before purpose", body: `{"expected_revision":"0","purpose":"open"}`, status: 400, code: "INVALID_ARGUMENT", field: "expected_revision"},
		{name: "invalid purpose", body: `{"expected_revision":"3","purpose":"PREVIEW"}`, status: 400, code: "INVALID_ARGUMENT", field: "purpose"},
		{name: "directory", serviceError: domain.ErrFileEntryNotFile, status: 400, calls: 1, code: "INVALID_ARGUMENT", field: "id"},
		{name: "service purpose", serviceError: domain.ErrInvalidDownloadPurpose, status: 400, calls: 1, code: "INVALID_ARGUMENT", field: "purpose"},
		{name: "missing or hidden file", serviceError: fmt.Errorf("private-storage: %w", domain.ErrFileEntryNotFound),
			status: 404, calls: 1, code: "ENTRY_NOT_FOUND"},
		{name: "changed revision", serviceError: fmt.Errorf("private-storage: %w", &domain.EntryRevisionConflictError{CurrentRevision: math.MaxUint64}),
			status: 409, calls: 1, code: "REVISION_CONFLICT"},
		{name: "unusable conflict revision", serviceError: &domain.EntryRevisionConflictError{}, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "missing conflict revision", serviceError: domain.ErrEntryRevisionConflict, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "object unavailable", serviceError: domain.ErrDownloadTargetUnavailable, status: 409, calls: 1, code: "TARGET_UNAVAILABLE"},
		{name: "unsupported preview", serviceError: domain.ErrUnsupportedPreview, status: 422, calls: 1, code: "UNSUPPORTED_PREVIEW"},
		{name: "storage unavailable", serviceError: fmt.Errorf("private-storage: %w", domain.ErrDownloadStorageUnavailable),
			status: 503, calls: 1, code: "STORAGE_UNAVAILABLE"},
		{name: "database unavailable", serviceError: fmt.Errorf("private-storage: %w", domain.ErrDownloadDatabaseUnavailable),
			status: 503, calls: 1, code: "DATABASE_UNAVAILABLE"},
		{name: "disabled account", serviceError: domain.ErrAccountDisabled, status: 403, calls: 1, code: "ACCOUNT_DISABLED"},
		{name: "revoked session", serviceError: domain.ErrSessionRevoked, status: 401, calls: 1, code: "SESSION_REVOKED"},
		{name: "unknown failure", serviceError: errors.New("private-storage"), status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "nil success", nilResult: true, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "wrong result version", wrongRevision: true, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "cancel before service", cancelOn: 2, status: 500, code: "INTERNAL_ERROR"},
		{name: "cancel after service", cancelOn: 3, status: 500, calls: 1, code: "INTERNAL_ERROR", serviceError: domain.ErrDownloadStorageUnavailable},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 挂载真实授权中间件及测试服务，核对信任来源和完整标识。
			actor := domain.Actor{UserID: 7, DeviceID: 19, SessionID: 17}
			entryID, revision, purpose := scenario.entryID, scenario.revision, scenario.purpose
			if entryID == 0 {
				entryID = 23
			}
			if revision == 0 {
				revision = 3
			}
			if purpose == "" {
				purpose = "download"
			}
			result := createDownloadSuccessFixture()
			result.Revision = revision
			if purpose == "preview" {
				result.Disposition = strings.Replace(result.Disposition, "attachment", "inline", 1)
			}
			if scenario.wrongRevision {
				result.Revision++
			}
			if scenario.nilResult {
				result = nil
			}
			calls := 0
			var requestScope context.Context
			create := handler.CreateDownload(func(ctx context.Context, identity domain.Actor, targetID, expectedRevision uint64, requestedPurpose string) (*domain.EntryDownloadAuthorization, error) {
				calls++
				if ctx != requestScope || identity != actor || targetID != entryID || expectedRevision != revision || requestedPurpose != purpose ||
					requestcontext.RequestID(ctx) != requestID {
					t.Error("下载服务未收到当前请求上下文、身份、完整路径和正文")
				}
				return result, scenario.serviceError
			})
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			auth := middleware.RequireAuthorization(func(context.Context, string) (*domain.Actor, error) { return &actor, nil }, handler.AuthorizationFailure)
			router.POST(route, auth, func(requestContext *gin.Context) {
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
				requestScope = requestContext.Request.Context()
				if scenario.cancelOn != 0 {
					ctx, cancel := context.WithCancel(requestScope)
					defer cancel()
					requestScope = &cancelUserSessionsContext{Context: ctx, cancel: cancel, cancelOn: scenario.cancelOn}
					requestContext.Request = requestContext.Request.WithContext(requestScope)
				}
				create(requestContext)
			}, func(*gin.Context) { t.Error("下载交付后执行了下游") })

			// 2. 发出完整 HTTP 请求，核对提前拒绝及公开成功或错误正文。
			path, body := scenario.path, scenario.body
			if path == "" {
				path = strconv.FormatUint(entryID, 10)
			}
			if body == "" {
				body = defaultBody
			}
			if scenario.oversized {
				body = strings.Repeat(" ", 1<<20) + body
			}
			request := httptest.NewRequest(http.MethodPost, "/test-download/"+path+"?user_id=99", strings.NewReader(body))
			request.Header.Set("X-Request-ID", requestID)
			request.Header.Set("X-User-ID", "99")
			request.Header.Set("Idempotency-Key", "unused")
			request.Header.Set("Content-Type", "application/json")
			if !scenario.noBearer {
				request.Header.Set("Authorization", "Bearer private-credential")
			}
			if scenario.invalidType {
				request.Header.Set("Content-Type", "text/plain")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != scenario.status || calls != scenario.calls ||
				response.Header().Get("Content-Encoding") != "" || response.Header().Get("Cache-Control") != "no-store" ||
				response.Header().Get("X-Request-ID") != requestID || (response.Header().Get("WWW-Authenticate") != "") != (scenario.status == 401) {
				t.Fatal("下载响应状态、调用次数或响应头不符", response.Code, calls)
			}
			if scenario.status == http.StatusOK {
				var success dto.CreateDownloadResponse
				if json.Unmarshal(response.Body.Bytes(), &success) != nil || success.RequestID != requestID ||
					success.Data.Revision != strconv.FormatUint(revision, 10) || success.Data.URL != result.Link.URL {
					t.Fatal("下载成功正文与内部授权不符")
				}
				return
			}
			var expectedDetails map[string]any
			if scenario.field != "" {
				expectedDetails = map[string]any{"field": scenario.field}
			} else if scenario.code == "REVISION_CONFLICT" {
				expectedDetails = map[string]any{"current_revision": "18446744073709551615"}
			}
			var failure dto.ErrorResponse
			if json.Unmarshal(response.Body.Bytes(), &failure) != nil || failure.RequestID != requestID ||
				failure.Error.Code != scenario.code || !reflect.DeepEqual(failure.Error.Details, expectedDetails) ||
				strings.Contains(response.Body.String(), "private-") || strings.Contains(response.Body.String(), `"data"`) {
				t.Fatal("下载错误正文不符或包含敏感信息", failure.Error.Code)
			}
		})
	}
}
