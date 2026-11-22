package http

import (
	"bytes"
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
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

func TestCreateSyncSnapshotHandlerFlow(t *testing.T) {
	const route = "/test-sync-snapshots"
	const key = "Fixture-Create-Snapshot"
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	const privateText = "fixture-internal-diagnostic"
	actor := domain.Actor{UserID: 7, DeviceID: 19, SessionID: 91}
	for _, testCase := range []struct {
		name, body, token, contentType string
		noKey                          bool
		status, pageSize               int
		code, field                    string
		serviceError                   error
	}{
		{name: "default page size", status: 201, pageSize: 50},
		{name: "full first page", body: `{"page_size":1}`, status: 201, pageSize: 1},
		{name: "missing authentication first", token: "none", noKey: true, body: "broken JSON", status: 401, code: "AUTH_REQUIRED"},
		{name: "disabled account first", token: "disabled", noKey: true, status: 403, code: "ACCOUNT_DISABLED"},
		{name: "missing idempotency key", noKey: true, status: 400, code: "INVALID_ARGUMENT", field: "Idempotency-Key"},
		{name: "invalid page size", body: `{"page_size":101}`, status: 400, code: "INVALID_ARGUMENT", field: "page_size"},
		{name: "duplicate page size", body: `{"page_size":1,"page_size":2}`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "request too large", body: "{}" + strings.Repeat(" ", int(binding.MaxJSONBodyBytes)), status: 413, code: "REQUEST_BODY_TOO_LARGE"},
		{name: "unsupported content type", contentType: "text/plain", status: 415, code: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "snapshot is too large", serviceError: domain.ErrSyncSnapshotTooLarge, pageSize: 50, status: 422, code: "SNAPSHOT_TOO_LARGE"},
		{name: "no cache budget", serviceError: domain.ErrSyncSnapshotBusy, pageSize: 50, status: 503, code: "SNAPSHOT_BUSY"},
		{name: "different parameters", serviceError: domain.ErrIdempotencyRequestConflict, pageSize: 50, status: 409, code: "IDEMPOTENCY_CONFLICT"},
		{name: "expired receipt", serviceError: domain.ErrIdempotencyRecordExpired, pageSize: 50, status: 409, code: "IDEMPOTENCY_EXPIRED"},
		{name: "expired snapshot", serviceError: domain.ErrSyncSnapshotExpired, pageSize: 50, status: 410, code: "CURSOR_EXPIRED"},
		{name: "account disabled while creating", serviceError: domain.ErrAccountDisabled, pageSize: 50, status: 403, code: "ACCOUNT_DISABLED"},
		{name: "device revoked while creating", serviceError: domain.ErrDeviceRevoked, pageSize: 50, status: 401, code: "SESSION_REVOKED"},
		{name: "unexpected error", serviceError: errors.New(privateText), pageSize: 50, status: 500, code: "INTERNAL_ERROR"},
		{name: "request canceled during service", pageSize: 50, status: 500, code: "INTERNAL_ERROR"},
		{name: "missing service result", pageSize: 50, status: 500, code: "INTERNAL_ERROR"},
		{name: "oversized service result", body: `{"page_size":1}`, pageSize: 1, status: 500, code: "INTERNAL_ERROR"},
		{name: "short first page with next cursor", body: `{"page_size":2}`, pageSize: 2, status: 500, code: "INTERNAL_ERROR"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 在测试路由上复用 Bearer 中间件，核对传给创建方法的输入。
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var logs bytes.Buffer
			router := NewRouter(logging.New(&logs), nil, func(context.Context) bool { return true })
			authorize := middleware.RequireAuthorization(func(_ context.Context, token string) (*domain.Actor, error) {
				if token == "disabled" {
					return nil, domain.ErrAccountDisabled
				}
				return &actor, nil
			}, handler.AuthorizationFailure)
			calls := 0
			page := syncSnapshotPageSuccessFixture()
			if testCase.name == "default page size" {
				page.NextCursor, page.BaselineToken = nil, new("fixture-baseline.fixture-signature")
			}
			router.POST(route, authorize, handler.CreateSyncSnapshot(func(callContext context.Context, identity domain.Actor, requestKey string, pageSize int) (*domain.SyncSnapshotPageResult, error) {
				calls++
				if identity != actor || requestKey != key || pageSize != testCase.pageSize || requestcontext.RequestID(callContext) != requestID {
					t.Fatal("创建服务收到的身份、幂等键、页大小或请求 ID 不符")
				}
				if testCase.serviceError != nil {
					return nil, fmt.Errorf("%s: %w", privateText, testCase.serviceError)
				}
				switch testCase.name {
				case "request canceled during service":
					cancel()
				case "missing service result":
					return nil, nil
				case "oversized service result":
					page.Items = append(page.Items, page.Items[0])
				}
				return page, nil
			}), func(*gin.Context) { t.Error("响应已结束后进入下游") })

			// 2. 发送请求，核对公开状态、错误详情及服务调用次数。
			body, token, contentType := testCase.body, testCase.token, testCase.contentType
			if body == "" {
				body = "{}"
			}
			if token == "" {
				token = "owner"
			}
			if contentType == "" {
				contentType = "application/json"
			}
			request := httptest.NewRequest(http.MethodPost, route, strings.NewReader(body)).WithContext(ctx)
			request.Header.Set("Content-Type", contentType)
			request.Header.Set("X-Request-ID", requestID)
			if token != "none" {
				request.Header.Set("Authorization", "Bearer "+token)
			}
			if !testCase.noKey {
				request.Header.Set("Idempotency-Key", " "+key+"\t")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			wantCalls := 0
			if testCase.pageSize != 0 {
				wantCalls = 1
			}
			if response.Code != testCase.status || calls != wantCalls || response.Header().Get("X-Request-ID") != requestID ||
				response.Header().Get("Cache-Control") != "no-store" ||
				strings.Contains(response.Body.String(), privateText) || strings.Contains(logs.String(), key) {
				t.Fatalf("状态=%d，调用=%d，正文=%s", response.Code, calls, response.Body.String())
			}
			if testCase.status == http.StatusCreated {
				var body dto.SyncSnapshotPageResponse
				if json.Unmarshal(response.Body.Bytes(), &body) != nil || body.RequestID != requestID ||
					body.Data.SnapshotID != page.SnapshotID.String() || len(body.Data.Items) != len(page.Items) {
					t.Fatalf("首页响应不符：%s", response.Body.String())
				}
				return
			}
			var failure dto.ErrorResponse
			if json.Unmarshal(response.Body.Bytes(), &failure) != nil || failure.RequestID != requestID ||
				failure.Error.Code != testCase.code || (testCase.field != "" && failure.Error.Details["field"] != testCase.field) ||
				(testCase.field == "" && failure.Error.Details != nil) {
				t.Fatalf("错误响应不符：%s", response.Body.String())
			}
		})
	}
}
