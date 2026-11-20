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

func TestPurgePreviewHandlerFlow(t *testing.T) {
	const prefix = "/test-trash/"
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	const validID = "18446744073709551615"
	for _, scenario := range []struct {
		name, pathID, code, field string
		status, calls             int
		serviceError              error
	}{
		{name: "maximum ID success", status: 200, calls: 1},
		{name: "missing bearer before invalid ID", pathID: "0", status: 401, code: "AUTH_REQUIRED"},
		{name: "invalid bearer before invalid ID", pathID: "0", status: 401, code: "AUTH_REQUIRED"},
		{name: "zero ID", pathID: "0", status: 400, code: "INVALID_ARGUMENT", field: "id"},
		{name: "leading zero", pathID: "014", status: 400, code: "INVALID_ARGUMENT", field: "id"},
		{name: "overflow", pathID: "18446744073709551616", status: 400, code: "INVALID_ARGUMENT", field: "id"},
		{name: "subtree limit", status: 400, code: "INVALID_ARGUMENT", field: "id", calls: 1, serviceError: domain.ErrFileEntrySubtreeTooLarge},
		{name: "true root", status: 403, code: "ROOT_OPERATION_FORBIDDEN", calls: 1, serviceError: domain.ErrRootEntryPurge},
		{name: "unavailable entry", status: 404, code: "ENTRY_NOT_FOUND", calls: 1, serviceError: domain.ErrFileEntryNotFound},
		{name: "account disabled", status: 403, code: "ACCOUNT_DISABLED", calls: 1, serviceError: domain.ErrAccountDisabled},
		{name: "internal failure", status: 500, code: "INTERNAL_ERROR", calls: 1, serviceError: errors.New("database lookup failed")},
		{name: "missing actor", status: 500, code: "INTERNAL_ERROR"},
		{name: "nil result", status: 500, code: "INTERNAL_ERROR", calls: 1},
		{name: "mismatched target", status: 500, code: "INTERNAL_ERROR", calls: 1},
		{name: "canceled before service", status: 500, code: "INTERNAL_ERROR"},
		{name: "canceled during service", status: 500, code: "INTERNAL_ERROR", calls: 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 接入共享认证中间件，核对预检服务收到的身份和路径目标。
			actor := domain.Actor{UserID: 7, SessionID: 17, DeviceID: 19}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			authorize := middleware.RequireAuthorization(func(_ context.Context, token string) (*domain.Actor, error) {
				if token != "unit-access" {
					return nil, domain.ErrInvalidAccessToken
				}
				return &actor, nil
			}, handler.AuthorizationFailure)
			result := purgePreviewSuccessFixture(t)
			preview := handler.PurgePreview(func(serviceContext context.Context, identity domain.Actor, entryID uint64) (*domain.PurgePreview, error) {
				calls++
				if identity != actor || entryID != math.MaxUint64 || requestcontext.RequestID(serviceContext) != requestID {
					t.Error("预检服务未收到认证身份、路径 ID 或当次请求 ID")
				}
				switch scenario.name {
				case "nil result":
					return nil, nil
				case "mismatched target":
					result.EntryID--
				case "canceled during service":
					cancel()
				}
				if scenario.serviceError != nil {
					return nil, fmt.Errorf("private-service-detail: %w", scenario.serviceError)
				}
				return result, nil
			})
			if scenario.name == "missing actor" {
				authorize = func(*gin.Context) {}
			}
			router.GET(prefix+":id/purge-preview", authorize, func(requestContext *gin.Context) {
				if scenario.name == "canceled before service" {
					cancel()
				}
				requestContext.Header("Content-Encoding", "gzip")
				requestContext.Header("Content-Length", "3")
			}, preview, func(*gin.Context) { t.Error("预检响应后仍执行下游") })

			// 2. 以路径和认证上下文为准，查询参数和自定义头不参与选择。
			pathID := scenario.pathID
			if pathID == "" {
				pathID = validID
			}
			request := httptest.NewRequest(http.MethodGet, prefix+pathID+"/purge-preview?id=99&user_id=99", nil).WithContext(ctx)
			request.Header.Set("X-Request-ID", requestID)
			request.Header.Set("X-User-ID", "99")
			switch scenario.name {
			case "missing bearer before invalid ID":
			case "invalid bearer before invalid ID":
				request.Header.Set("Authorization", "Bearer invalid-access")
			default:
				request.Header.Set("Authorization", "Bearer unit-access")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			header := response.Header()
			if response.Code != scenario.status || calls != scenario.calls || header.Get("X-Request-ID") != requestID ||
				header.Get("Content-Type") != "application/json; charset=utf-8" ||
				header.Get("Cache-Control") != "no-store" || header.Get("Content-Encoding") != "" ||
				(scenario.code != "AUTH_REQUIRED" && header.Get("Pragma") != "no-cache") ||
				(scenario.status == 401) != (header.Get("WWW-Authenticate") == `Bearer realm="cloudnest"`) {
				t.Fatalf("预检状态 %d，服务调用 %d 次，响应头 %v", response.Code, calls, header)
			}
			if response.Code == http.StatusOK {
				var body dto.PurgePreviewResponse
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.RequestID != requestID ||
					body.Data.EntryID != validID || body.Data.Revision != "4" || body.Data.PreviewDigest != result.Credential.PreviewDigest {
					t.Fatal("成功响应的目标、版本或凭证错误", err)
				}
				return
			}
			var failure dto.ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil || failure.RequestID != requestID ||
				failure.Error.Code != scenario.code || (scenario.field == "" && failure.Error.Details != nil) ||
				(scenario.field != "" && (len(failure.Error.Details) != 1 || failure.Error.Details["field"] != scenario.field)) ||
				strings.Contains(response.Body.String(), "private-service-detail") ||
				strings.Contains(response.Body.String(), result.Credential.PreviewDigest) {
				t.Fatal("公开错误未按约定交付", failure.Error.Code, failure.Error.Details, err)
			}
		})
	}
}
