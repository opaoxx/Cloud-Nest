package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func TestPurgeEntryHandlerFlow(t *testing.T) {
	const path = "/test-trash/"
	const entryIDText = "18446744073709551615"
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	const previewHeader = dto.PurgePreviewDigestHeader
	const revisionHeader = dto.PurgeExpectedRevisionHeader
	digest := purgePreviewSuccessFixture(t).Credential.PreviewDigest

	for _, scenario := range []struct {
		name, rawID, code, field string
		overrideHeaders          map[string][]string
		status, calls            int
		serviceError             error
		result                   string
		cancelOn                 int
	}{
		{name: "accepted", status: 202, calls: 1},
		{name: "authenticate first", rawID: "0", overrideHeaders: map[string][]string{"Authorization": nil, "Idempotency-Key": nil}, status: 401, code: "AUTH_REQUIRED"},
		{name: "path before headers", rawID: "0", overrideHeaders: map[string][]string{"Idempotency-Key": nil}, status: 400, code: "INVALID_ARGUMENT", field: "id"},
		{name: "idempotency key before preview headers", overrideHeaders: map[string][]string{"Idempotency-Key": nil, revisionHeader: nil}, status: 400, code: "INVALID_ARGUMENT", field: "Idempotency-Key"},
		{name: "duplicate idempotency key", overrideHeaders: map[string][]string{"Idempotency-Key": {"Purge-Key", "other"}}, status: 400, code: "INVALID_ARGUMENT", field: "Idempotency-Key"},
		{name: "revision before digest", overrideHeaders: map[string][]string{revisionHeader: {"18446744073709551616"}, previewHeader: nil}, status: 400, code: "INVALID_ARGUMENT", field: revisionHeader},
		{name: "duplicate revision", overrideHeaders: map[string][]string{revisionHeader: {"4", "4"}}, status: 400, code: "INVALID_ARGUMENT", field: revisionHeader},
		{name: "missing digest", overrideHeaders: map[string][]string{previewHeader: nil}, status: 400, code: "INVALID_ARGUMENT", field: previewHeader},
		{name: "duplicate digest", overrideHeaders: map[string][]string{previewHeader: {digest, digest}}, status: 400, code: "INVALID_ARGUMENT", field: previewHeader},
		{name: "invalid digest signature or scope", serviceError: domain.ErrInvalidPurgePreviewDigest, status: 400, calls: 1, code: "INVALID_ARGUMENT", field: previewHeader},
		{name: "true root", serviceError: domain.ErrRootEntryPurge, status: 403, calls: 1, code: "ROOT_OPERATION_FORBIDDEN"},
		{name: "unavailable target", serviceError: domain.ErrFileEntryNotFound, status: 404, calls: 1, code: "ENTRY_NOT_FOUND"},
		{name: "preview changed", serviceError: domain.ErrPurgePreviewChanged, status: 409, calls: 1, code: "PURGE_PREVIEW_CHANGED"},
		{name: "version changed", serviceError: &domain.EntryRevisionConflictError{CurrentRevision: math.MaxUint64}, status: 409, calls: 1, code: "REVISION_CONFLICT"},
		{name: "key conflict", serviceError: domain.ErrIdempotencyRequestConflict, status: 409, calls: 1, code: "IDEMPOTENCY_CONFLICT"},
		{name: "key expired", serviceError: domain.ErrIdempotencyRecordExpired, status: 409, calls: 1, code: "IDEMPOTENCY_EXPIRED"},
		{name: "session revoked inside transaction", serviceError: domain.ErrSessionRevoked, status: 401, calls: 1, code: "SESSION_REVOKED"},
		{name: "account disabled inside transaction", serviceError: domain.ErrAccountDisabled, status: 403, calls: 1, code: "ACCOUNT_DISABLED"},
		{name: "exhausted version", serviceError: domain.ErrEntryRevisionExhausted, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "unexpected service failure", serviceError: errors.New("database rejected request"), status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "invalid snapshot", result: `{}`, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "valid snapshot for wrong entry", result: `{"entry_id":"23","status":"PURGING","progress_url":"/api/v1/trash/23/purge"}`, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "canceled before service", cancelOn: 2, status: 500, code: "INTERNAL_ERROR"},
		{name: "canceled after service", cancelOn: 3, status: 500, calls: 1, code: "INTERNAL_ERROR"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 挂载真实 Bearer 中间件，以替代服务观察传入参数。
			actor := domain.Actor{UserID: 7, DeviceID: 19, SessionID: 17}
			calls := 0
			purge := handler.PurgeEntry(func(ctx context.Context, identity domain.Actor, entryID, revision uint64, credential, key string) (json.RawMessage, error) {
				calls++
				if identity != actor || entryID != math.MaxUint64 || revision != math.MaxUint64-1 ||
					credential != digest || key != "Purge-Key" || requestcontext.RequestID(ctx) != requestID {
					t.Fatal("受理服务收到错误的身份、目标、版本、凭证或幂等键")
				}
				if scenario.serviceError != nil {
					return nil, fmt.Errorf("private-service-detail: %w", scenario.serviceError)
				}
				if scenario.result != "" {
					return json.RawMessage(scenario.result), nil
				}
				return json.RawMessage(purgeAcceptedTestBody), nil
			})
			var logs bytes.Buffer
			router := NewRouter(logging.New(&logs), nil, func(context.Context) bool { return true })
			auth := middleware.RequireAuthorization(func(_ context.Context, token string) (*domain.Actor, error) {
				if token != "private-access-token" {
					return nil, domain.ErrInvalidAccessToken
				}
				return &actor, nil
			}, handler.AuthorizationFailure)
			router.DELETE(path+":id", auth, func(requestContext *gin.Context) {
				requestContext.Header("Content-Encoding", "gzip")
				requestContext.Header("Content-Length", "3")
				if scenario.cancelOn != 0 {
					ctx, cancel := context.WithCancel(requestContext.Request.Context())
					defer cancel()
					requestContext.Request = requestContext.Request.WithContext(&cancelUserSessionsContext{Context: ctx, cancel: cancel, cancelOn: scenario.cancelOn})
				}
				purge(requestContext)
			}, func(*gin.Context) { t.Error("永久清理响应后继续执行了下游") })

			// 2. 发出确认请求，核对成功数据、公开错误和服务调用次数。
			rawID := scenario.rawID
			if rawID == "" {
				rawID = entryIDText
			}
			request := httptest.NewRequest(http.MethodDelete, path+rawID+"?id=23&user_id=99&expected_revision=1", nil)
			request.Header.Set("X-User-ID", "99")
			request.Header.Set("X-Request-ID", requestID)
			request.Header.Set("Authorization", "Bearer private-access-token")
			request.Header.Set("Idempotency-Key", " \tPurge-Key\t ")
			request.Header.Set(revisionHeader, strconv.FormatUint(math.MaxUint64-1, 10))
			request.Header.Set(previewHeader, digest)
			for header, values := range scenario.overrideHeaders {
				request.Header.Del(header)
				for _, value := range values {
					request.Header.Add(header, value)
				}
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			header := response.Header()
			contentLength := header.Get("Content-Length")
			if response.Code != scenario.status || calls != scenario.calls || header.Get("X-Request-ID") != requestID ||
				header.Get("Content-Type") != "application/json; charset=utf-8" || header.Get("Content-Encoding") != "" ||
				(contentLength != "" && contentLength != strconv.Itoa(response.Body.Len())) ||
				header.Get("Cache-Control") != "no-store" || (scenario.code != "AUTH_REQUIRED" && header.Get("Pragma") != "no-cache") ||
				(scenario.status == http.StatusUnauthorized) != (header.Get("WWW-Authenticate") == `Bearer realm="cloudnest"`) {
				t.Fatalf("状态 %d，服务调用 %d 次，响应头 %v", response.Code, calls, header)
			}
			if strings.Contains(response.Body.String(), "private-") || strings.Contains(response.Body.String(), digest) ||
				strings.Contains(logs.String(), "private-") || strings.Contains(logs.String(), digest) || strings.Contains(logs.String(), "Purge-Key") {
				t.Fatal("公开响应或日志包含原始凭证、幂等键或内部错误")
			}
			if scenario.status == http.StatusAccepted {
				var success dto.PurgeEntryAcceptedResponse
				want := dto.PurgeEntryAcceptedData{EntryID: entryIDText, Status: "PURGING", ProgressURL: purgeAcceptedTestProgressURL}
				if err := json.Unmarshal(response.Body.Bytes(), &success); err != nil || success.RequestID != requestID || success.Data != want {
					t.Fatal("受理快照未正确交付", success, err)
				}
				return
			}

			var details map[string]any
			if scenario.field != "" {
				details = map[string]any{"field": scenario.field}
			} else if scenario.code == "REVISION_CONFLICT" {
				details = map[string]any{"current_revision": entryIDText}
			}
			var failure dto.ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil || failure.RequestID != requestID ||
				failure.Error.Code != scenario.code || !reflect.DeepEqual(failure.Error.Details, details) {
				t.Fatal("错误响应未按约定交付", failure, err)
			}
		})
	}
}
