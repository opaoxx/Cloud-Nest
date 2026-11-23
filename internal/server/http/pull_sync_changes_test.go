package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

func TestPullSyncChangesHandlerFlow(t *testing.T) {
	const requestID = "eb9c7469-d6e4-4f95-991c-a8341bcd8755"
	const path = "/test-sync-changes/pull"
	const cursor = "synthetic-cursor.signature"
	const baseline = "synthetic-baseline.signature"
	const nextCursor = "synthetic-next.signature"
	const diagnostic = "fixture-internal-pull-diagnostic"
	const defaultBody = `{"cursor":"` + cursor + `","page_size":2}`
	const baselineBody = `{"baseline_token":"` + baseline + `"}`
	const payload = `{"entry_id":"12","parent_id":"11","name":"目录","revision":"2"}`
	actor := domain.Actor{UserID: 7, SessionID: 91, DeviceID: 19}
	cursorInput := dto.PullSyncChangesInput{Cursor: cursor, PageSize: 2}
	baselineInput := dto.PullSyncChangesInput{BaselineToken: baseline, PageSize: 50}
	for _, scenario := range []struct {
		name, bearer, body, mediaType string
		status                        int
		code, field                   string
		input                         dto.PullSyncChangesInput
		serviceError                  error
		empty, invalidResult, cancel  bool
	}{
		{name: "first page", body: `{"baseline_token":"synthetic-baseline\u002esignature"}`, status: 200, input: baselineInput},
		{name: "cursor returns an empty page", status: 200, input: cursorInput, empty: true},
		{name: "missing authentication before body validation", bearer: "missing", body: "!", status: 401, code: "AUTH_REQUIRED"},
		{name: "disabled account before body validation", bearer: "disabled", body: "!", status: 403, code: "ACCOUNT_DISABLED"},
		{name: "both credentials", body: `{"baseline_token":"a.b","cursor":"c.d"}`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "invalid page size", body: `{"cursor":"a.b","page_size":0}`, status: 400, code: "INVALID_ARGUMENT", field: "page_size"},
		{name: "malformed cursor", body: `{"cursor":"a/b.c"}`, status: 400, code: "INVALID_CURSOR", field: "cursor"},
		{name: "oversized body", body: defaultBody + strings.Repeat(" ", int(binding.MaxJSONBodyBytes)), status: 413, code: "REQUEST_BODY_TOO_LARGE"},
		{name: "unsupported media type", mediaType: "text/plain", status: 415, code: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "baseline rejected by verifier", body: baselineBody, input: baselineInput, serviceError: domain.ErrInvalidSyncSnapshotBaselineToken,
			status: 400, code: "INVALID_CURSOR", field: "baseline_token"},
		{name: "cursor rejected by service", input: cursorInput, serviceError: domain.ErrInvalidSyncChangeCursor,
			status: 400, code: "INVALID_CURSOR", field: "cursor"},
		{name: "expired position", input: cursorInput, serviceError: domain.ErrSyncSnapshotExpired, status: 410, code: "CURSOR_EXPIRED"},
		{name: "session revoked during read", input: cursorInput, serviceError: domain.ErrSessionRevoked, status: 401, code: "SESSION_REVOKED"},
		{name: "account disabled during read", input: cursorInput, serviceError: domain.ErrAccountDisabled, status: 403, code: "ACCOUNT_DISABLED"},
		{name: "invalid log data", input: cursorInput, serviceError: domain.ErrInvalidSyncChangePage, status: 500, code: "INTERNAL_ERROR"},
		{name: "database error", input: cursorInput, serviceError: errors.New("fixture database unavailable"), status: 500, code: "INTERNAL_ERROR"},
		{name: "invalid success result", input: cursorInput, invalidResult: true, status: 500, code: "INTERNAL_ERROR"},
		{name: "canceled before delivery", input: cursorInput, cancel: true, status: 500, code: "INTERNAL_ERROR"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 用实际认证中间件和独立的测试路由核对传入拉取服务的参数。
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			router := gin.New()
			router.Use(middleware.RequestID())
			authorize := middleware.RequireAuthorization(func(_ context.Context, bearer string) (*domain.Actor, error) {
				if bearer == "disabled" {
					return nil, domain.ErrAccountDisabled
				}
				return &actor, nil
			}, handler.AuthorizationFailure)
			calls := 0
			pull := handler.PullSyncChanges(func(callContext context.Context, identity domain.Actor, baselineToken, cursor string, pageSize int) (*domain.SyncChangePullResult, error) {
				calls++
				got := dto.PullSyncChangesInput{BaselineToken: baselineToken, Cursor: cursor, PageSize: pageSize}
				if identity != actor || requestcontext.RequestID(callContext) != requestID || got != scenario.input {
					t.Fatal("拉取服务收到的身份、请求 ID 或正文参数不符")
				}
				result := &domain.SyncChangePullResult{HighWaterSequence: 22, NextCursor: nextCursor}
				if !scenario.empty {
					result.HasMore = true
					result.Events = []domain.SyncChangeEvent{
						{Sequence: 18, EntryID: 12, Revision: 2, EventType: "ENTRY_RENAMED", Payload: json.RawMessage(payload)},
					}
				}
				if scenario.invalidResult {
					result.NextCursor = ""
				}
				if scenario.cancel {
					cancel()
				}
				if scenario.serviceError != nil {
					return result, fmt.Errorf("%s: %w", diagnostic, scenario.serviceError)
				}
				return result, nil
			})
			router.POST(path, authorize, pull, func(*gin.Context) { t.Error("已完成响应仍进入下游") })

			// 2. 发送合成请求，核对响应头与服务调用次数。
			body, mediaType, bearer := scenario.body, scenario.mediaType, scenario.bearer
			if body == "" {
				body = defaultBody
			}
			if mediaType == "" {
				mediaType = "application/json"
			}
			if bearer == "" {
				bearer = "fixture-user"
			}
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)).WithContext(ctx)
			request.Header.Set("Content-Type", mediaType)
			request.Header.Set(middleware.RequestIDHeader, requestID)
			if bearer != "missing" {
				request.Header.Set("Authorization", "Bearer "+bearer)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			wantCalls := 0
			if scenario.input.PageSize != 0 {
				wantCalls = 1
			}
			if calls != wantCalls || response.Code != scenario.status ||
				response.Header().Get(middleware.RequestIDHeader) != requestID || response.Header().Get("Cache-Control") != "no-store" ||
				!strings.HasPrefix(response.Header().Get("Content-Type"), "application/json") {
				t.Fatalf("状态、响应头或调用次数不符：%d，%v，调用 %d 次", response.Code, response.Header(), calls)
			}

			// 3. 成功响应核对完整页面，错误响应核对业务码、字段与请求 ID。
			if scenario.status == http.StatusOK {
				var success dto.PullSyncChangesResponse
				if err := json.Unmarshal(response.Body.Bytes(), &success); err != nil || success.RequestID != requestID ||
					success.Data.HighWaterSequence != "22" || success.Data.NextCursor != nextCursor ||
					success.Data.Events == nil || success.Data.HasMore == scenario.empty {
					t.Fatalf("成功响应不符：%s，错误 %v", response.Body.String(), err)
				}
				if scenario.empty {
					if len(success.Data.Events) != 0 {
						t.Fatal("空页仍有事件")
					}
				} else if len(success.Data.Events) != 1 || success.Data.Events[0].Sequence != "18" ||
					string(success.Data.Events[0].Payload) != payload {
					t.Fatal("事件序号或载荷不符")
				}
				return
			}
			var failure struct {
				dto.ErrorResponse
				Data json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil || failure.RequestID != requestID ||
				failure.Error.Code != scenario.code || len(failure.Data) != 0 ||
				strings.Contains(response.Body.String(), diagnostic) || strings.Contains(response.Body.String(), cursor) ||
				strings.Contains(response.Body.String(), baseline) || strings.Contains(response.Body.String(), nextCursor) {
				t.Fatalf("错误响应不符：%s，错误 %v", response.Body.String(), err)
			}
			if (scenario.field == "" && failure.Error.Details != nil) ||
				(scenario.field != "" && failure.Error.Details["field"] != scenario.field) {
				t.Fatalf("错误字段不符：%+v，预期 %q", failure.Error.Details, scenario.field)
			}
			if scenario.status == http.StatusUnauthorized && response.Header().Get("WWW-Authenticate") == "" {
				t.Fatal("认证失败响应缺少认证提示头")
			}
		})
	}
}
