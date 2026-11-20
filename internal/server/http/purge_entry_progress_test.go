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

func TestPurgeEntryProgressHandlerFlow(t *testing.T) {
	const prefix = "/test-trash/"
	const validID = "18446744073709551615"
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	for _, scenario := range []struct {
		name, rawID, result, code, field string
		status, calls, cancelOn          int
		noBearer                         bool
		authFailure, serviceFailure      error
	}{
		{name: "purging", status: 200, calls: 1},
		{name: "purged", result: "completed", status: 200, calls: 1},
		{name: "bearer before invalid ID", rawID: "0", noBearer: true, status: 401, code: "AUTH_REQUIRED"},
		{name: "expired bearer", rawID: "0", authFailure: domain.ErrAccessTokenExpired, status: 401, code: "TOKEN_EXPIRED"},
		{name: "disabled account", authFailure: domain.ErrAccountDisabled, status: 403, code: "ACCOUNT_DISABLED"},
		{name: "zero ID", rawID: "0", status: 400, code: "INVALID_ARGUMENT", field: "id"},
		{name: "leading zero", rawID: "023", status: 400, code: "INVALID_ARGUMENT", field: "id"},
		{name: "path overflow", rawID: "18446744073709551616", status: 400, code: "INVALID_ARGUMENT", field: "id"},
		{name: "unavailable target", serviceFailure: domain.ErrFileEntryNotFound, status: 404, code: "ENTRY_NOT_FOUND", calls: 1},
		{name: "subtree limit", serviceFailure: domain.ErrFileEntrySubtreeTooLarge, status: 500, code: "INTERNAL_ERROR", calls: 1},
		{name: "subtree corruption", serviceFailure: domain.ErrInvalidFileEntryData, status: 500, code: "INTERNAL_ERROR", calls: 1},
		{name: "database failure", serviceFailure: errors.New("database rejected query"), status: 500, code: "INTERNAL_ERROR", calls: 1},
		{name: "missing result", result: "nil", status: 500, code: "INTERNAL_ERROR", calls: 1},
		{name: "wrong target", result: "wrong ID", status: 500, code: "INTERNAL_ERROR", calls: 1},
		{name: "invalid result count", result: "invalid count", status: 500, code: "INTERNAL_ERROR", calls: 1},
		{name: "canceled before service", cancelOn: 2, status: 500, code: "INTERNAL_ERROR"},
		{name: "canceled after service", cancelOn: 3, status: 500, code: "INTERNAL_ERROR", calls: 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 挂载共享认证，记录服务是否收到认证身份、路径和请求 ID。
			actor := domain.Actor{UserID: 7, SessionID: 17, DeviceID: 19}
			calls := 0
			auth := middleware.RequireAuthorization(func(_ context.Context, token string) (*domain.Actor, error) {
				if token != "private-access-token" {
					t.Fatal("未使用 Bearer 令牌")
				}
				return &actor, scenario.authFailure
			}, handler.AuthorizationFailure)
			progress := handler.PurgeEntryProgress(func(ctx context.Context, identity domain.Actor, entryID uint64) (*domain.PurgeProgress, error) {
				calls++
				if identity != actor || entryID != math.MaxUint64 || requestcontext.RequestID(ctx) != requestID {
					t.Fatal("进度服务收到错误的身份、目标或请求 ID")
				}
				if scenario.serviceFailure != nil {
					return nil, fmt.Errorf("private-service-detail: %w", scenario.serviceFailure)
				}
				result := &domain.PurgeProgress{EntryID: entryID, Status: "PURGING", RemainingCount: 2}
				switch scenario.result {
				case "completed":
					result.Status, result.RemainingCount = "PURGED", 0
				case "nil":
					return nil, nil
				case "wrong ID":
					result.EntryID = 42
				case "invalid count":
					result.RemainingCount = 0
				}
				return result, nil
			})
			var logs bytes.Buffer
			router := NewRouter(logging.New(&logs), nil, func(context.Context) bool { return true })
			router.GET(prefix+":id/purge", auth, func(requestContext *gin.Context) {
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
				if scenario.cancelOn != 0 {
					ctx, cancel := context.WithCancel(requestContext.Request.Context())
					defer cancel()
					requestContext.Request = requestContext.Request.WithContext(&cancelUserSessionsContext{Context: ctx, cancel: cancel, cancelOn: scenario.cancelOn})
				}
				progress(requestContext)
			}, func(*gin.Context) { t.Error("交付响应后继续执行了下游") })

			// 2. 发出查询，检查成功或固定错误以及下游调用次数。
			rawID := scenario.rawID
			if rawID == "" {
				rawID = validID
			}
			request := httptest.NewRequest(http.MethodGet, prefix+rawID+"/purge?id=42&user_id=99", nil)
			request.Header.Set("X-Request-ID", requestID)
			if !scenario.noBearer {
				request.Header.Set("Authorization", "Bearer private-access-token")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			header := response.Header()
			contentLength := header.Get("Content-Length")
			if response.Code != scenario.status || calls != scenario.calls || header.Get("X-Request-ID") != requestID ||
				header.Get("Content-Type") != "application/json; charset=utf-8" || header.Get("Content-Encoding") != "" ||
				(contentLength != "" && contentLength != strconv.Itoa(response.Body.Len())) || header.Get("Cache-Control") != "no-store" ||
				(!scenario.noBearer && scenario.authFailure == nil && header.Get("Pragma") != "no-cache") ||
				(header.Get("WWW-Authenticate") == `Bearer realm="cloudnest"`) != (scenario.status == http.StatusUnauthorized) {
				t.Fatal("响应状态、头或服务调用次数不符", response.Code, header, calls)
			}
			if strings.Contains(logs.String(), "private-") || strings.Contains(response.Body.String(), "private-") {
				t.Fatal("公开响应或完成日志泄漏凭证或底层错误")
			}
			if response.Code == http.StatusOK {
				var success dto.PurgeEntryProgressResponse
				want := dto.PurgeEntryProgressData{EntryID: validID, Status: "PURGING", RemainingCount: 2}
				if scenario.result == "completed" {
					want.Status, want.RemainingCount = "PURGED", 0
				}
				if err := json.Unmarshal(response.Body.Bytes(), &success); err != nil || success.RequestID != requestID || success.Data != want {
					t.Fatal("进度响应不符", success, err)
				}
				return
			}
			var failure dto.ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil || failure.RequestID != requestID ||
				failure.Error.Code != scenario.code || (scenario.field == "" && failure.Error.Details != nil) ||
				(scenario.field != "" && (len(failure.Error.Details) != 1 || failure.Error.Details["field"] != scenario.field)) ||
				strings.Contains(response.Body.String(), `"data"`) {
				t.Fatal("错误响应不符", failure, err)
			}
		})
	}
}
