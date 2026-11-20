package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

func TestTrashPageHandlerFlow(t *testing.T) {
	const path = "/test-trash-page"
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	for _, testCase := range []struct {
		name, raw, cursor, code, field string
		status, calls, pageSize        int
		missingBearer, badBearer       bool
		serviceError                   error
	}{
		{name: "default first page", status: 200, calls: 1, pageSize: 50},
		{name: "decoded cursor and page size", raw: "cursor=A%2BB&limit=2", cursor: "A+B", status: 200, calls: 1, pageSize: 2},
		{name: "budget empty page", raw: "limit=1", status: 200, calls: 1, pageSize: 1},
		{name: "missing bearer before malformed query", raw: "note=%ZZ", missingBearer: true, status: 401, code: "AUTH_REQUIRED"},
		{name: "bad bearer before page size", raw: "limit=0", badBearer: true, status: 401, code: "AUTH_REQUIRED"},
		{name: "malformed query", raw: "note=%ZZ", status: 400, code: "INVALID_ARGUMENT", field: "query"},
		{name: "repeated cursor", raw: "cursor=x&cursor=y", status: 400, code: "INVALID_ARGUMENT", field: "cursor"},
		{name: "invalid page size", raw: "limit=0", status: 400, code: "INVALID_ARGUMENT", field: "limit"},
		{name: "invalid cursor from service", raw: "cursor=private-cursor", cursor: "private-cursor", pageSize: 50, calls: 1,
			status: 400, code: "INVALID_CURSOR", field: "cursor", serviceError: fmt.Errorf("private-detail: %w", domain.ErrInvalidTrashEntryCursor)},
		{name: "disabled account", pageSize: 50, calls: 1, status: 403, code: "ACCOUNT_DISABLED", serviceError: domain.ErrAccountDisabled},
		{name: "revoked session", pageSize: 50, calls: 1, status: 401, code: "SESSION_REVOKED", serviceError: domain.ErrSessionRevoked},
		{name: "internal error", pageSize: 50, calls: 1, status: 500, code: "INTERNAL_ERROR", serviceError: errors.New("private-detail")},
		{name: "nil result", pageSize: 50, calls: 1, status: 500, code: "INTERNAL_ERROR"},
		{name: "foreign result owner", pageSize: 50, calls: 1, status: 500, code: "INTERNAL_ERROR"},
		{name: "too many results", raw: "limit=1", pageSize: 1, calls: 1, status: 500, code: "INTERNAL_ERROR"},
		{name: "canceled during service", pageSize: 50, calls: 1, status: 500, code: "INTERNAL_ERROR"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 接入真实授权中间件，用替身记录分页调用参数。
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			actor := domain.Actor{UserID: 7, SessionID: 17, DeviceID: 19}
			calls := 0
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			authorize := middleware.RequireAuthorization(func(_ context.Context, token string) (*domain.Actor, error) {
				if token != "test-token" {
					return nil, domain.ErrInvalidAccessToken
				}
				return &actor, nil
			}, handler.AuthorizationFailure)
			handle := handler.TrashPage(func(serviceContext context.Context, who domain.Actor, pageSize int, cursor string) (*domain.TrashEntryPageResult, error) {
				calls++
				if who != actor || cursor != testCase.cursor || pageSize != testCase.pageSize || requestcontext.RequestID(serviceContext) != requestID {
					t.Error("分页服务未收到当前身份、参数或请求 ID")
				}
				if testCase.serviceError != nil || testCase.name == "nil result" {
					return nil, testCase.serviceError
				}
				result := trashPageSuccessFixture()
				switch testCase.name {
				case "budget empty page":
					result.Entries = nil
				case "foreign result owner":
					result.Entries[0].Entry.UserID = 99
					for index := range result.Entries[0].OriginalPath.Entries {
						result.Entries[0].OriginalPath.Entries[index].UserID = 99
					}
				case "too many results":
					result.Entries = append(result.Entries, result.Entries[0])
				case "canceled during service":
					cancel()
				}
				return result, nil
			})
			router.GET(path, authorize, handle, func(*gin.Context) { t.Error("响应后仍执行下游") })

			// 2. 请求中的 user_id 和头部标识不参与认证身份选择。
			request := httptest.NewRequest(http.MethodGet, path+"?user_id=99&"+testCase.raw, nil).WithContext(ctx)
			request.Header.Set("X-Request-ID", requestID)
			request.Header.Set("X-User-ID", "99")
			switch {
			case testCase.badBearer:
				request.Header.Set("Authorization", "Bearer bad-token")
			case !testCase.missingBearer:
				request.Header.Set("Authorization", "Bearer test-token")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			header := response.Header()
			if response.Code != testCase.status || calls != testCase.calls || header.Get("X-Request-ID") != requestID ||
				header.Get("Cache-Control") != "no-store" || header.Get("Content-Type") != "application/json; charset=utf-8" {
				t.Fatalf("分页状态为 %d、调用 %d 次、响应头为 %v", response.Code, calls, header)
			}
			if !testCase.missingBearer && !testCase.badBearer && header.Get("Pragma") != "no-cache" {
				t.Fatal("缺少回收站响应的缓存控制头")
			}
			if testCase.status == http.StatusOK {
				var body dto.TrashPageResponse
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.RequestID != requestID ||
					body.Data.Items == nil || body.Data.NextCursor == nil ||
					(len(body.Data.Items) == 0) != (testCase.name == "budget empty page") {
					t.Fatalf("公开分页响应为 %s，错误为 %v", response.Body.String(), err)
				}
				return
			}
			var body dto.ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.RequestID != requestID ||
				body.Error.Code != testCase.code || strings.Contains(response.Body.String(), "private-") {
				t.Fatalf("公开错误为 %s，错误为 %v", response.Body.String(), err)
			}
			if testCase.field != "" && body.Error.Details["field"] != testCase.field {
				t.Fatalf("字段错误定位为 %v，期望 %s", body.Error.Details, testCase.field)
			}
			if testCase.status == http.StatusUnauthorized && header.Get("WWW-Authenticate") != `Bearer realm="cloudnest"` {
				t.Fatal("401 缺少共享认证提示")
			}
		})
	}
}

func TestTrashPageHandlerStops(t *testing.T) {
	const path = "/test-trash-page"
	for _, scenario := range []string{"missing actor", "already written", "already aborted"} {
		t.Run(scenario, func(t *testing.T) {
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			handle := handler.TrashPage(func(context.Context, domain.Actor, int, string) (*domain.TrashEntryPageResult, error) {
				t.Fatal("请求提前结束后调用分页服务")
				return nil, nil
			})
			status := http.StatusInternalServerError
			body := ""
			router.GET(path, func(requestContext *gin.Context) {
				// 1. 无认证身份或响应已结束时，处理函数必须提前退出。
				switch scenario {
				case "already written":
					status, body = http.StatusConflict, "existing response"
					requestContext.String(status, body)
				case "already aborted":
					status = http.StatusConflict
					requestContext.AbortWithStatus(status)
				}
				handle(requestContext)
			}, func(*gin.Context) { t.Error("已结束请求继续执行") })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
			if response.Code != status || (status == http.StatusConflict && response.Body.String() != body) {
				t.Fatalf("提前结束后状态为 %d，正文为 %s", response.Code, response.Body.String())
			}
		})
	}
	t.Run("missing service dependency", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("缺少分页服务时未拒绝装配")
			}
		}()
		handler.TrashPage(nil)
	})
}
