package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

func TestConfirmSyncChangesHandlerFlow(t *testing.T) {
	const path = "/test-sync-changes/confirm"
	const requestID = "eb9c7469-d6e4-4f95-991c-a8341bcd8755"
	const cursor = "synthetic-confirmation.signature"
	const body = `{"cursor":"` + cursor + `"}`
	const diagnostic = "fixture-internal-sync-confirmation-detail"
	actor := domain.Actor{UserID: 7, DeviceID: 19, SessionID: 27}
	for _, scenario := range []struct {
		name, bearer, body string
		status             int
		code, field        string
		serviceCalls       int
		ack                uint64
		ackText            string
		serviceError       error
		cancelAfter        bool
	}{
		{name: "committed zero position", status: 200, serviceCalls: 1, ackText: "0"},
		{name: "maximum position with escaped cursor", body: `{"cursor":"synthetic-confirmation\u002esignature"}`,
			status: 200, serviceCalls: 1, ack: math.MaxUint64, ackText: "18446744073709551615"},
		{name: "missing Bearer before malformed body", bearer: "missing", body: "!", status: 401, code: "AUTH_REQUIRED"},
		{name: "disabled before malformed body", bearer: "disabled", body: "!", status: 403, code: "ACCOUNT_DISABLED"},
		{name: "extra position field", body: `{"cursor":"a.b","ack_sequence":"19"}`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "missing cursor", body: "{}", status: 400, code: "INVALID_CURSOR", field: "cursor"},
		{name: "cursor rejected in service", status: 400, code: "INVALID_CURSOR", field: "cursor",
			serviceCalls: 1, serviceError: domain.ErrInvalidSyncChangeCursor},
		{name: "expired cursor", status: 410, code: "CURSOR_EXPIRED", serviceCalls: 1, serviceError: domain.ErrSyncSnapshotExpired},
		{name: "session revoked in transaction", status: 401, code: "SESSION_REVOKED", serviceCalls: 1, serviceError: domain.ErrSessionRevoked},
		{name: "account disabled in transaction", status: 403, code: "ACCOUNT_DISABLED", serviceCalls: 1, serviceError: domain.ErrAccountDisabled},
		{name: "commit result unknown", status: 500, code: "INTERNAL_ERROR", serviceCalls: 1, ack: 19,
			serviceError: domain.ErrUserTransactionCommitUnconfirmed},
		{name: "database failure", status: 500, code: "INTERNAL_ERROR", serviceCalls: 1, serviceError: errors.New("fixture persistence failure")},
		{name: "request canceled before delivery", status: 500, code: "INTERNAL_ERROR", serviceCalls: 1, ack: 19, cancelAfter: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 在独立路由用共享认证注入合成身份，确认方法只接收合法参数。
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			router := gin.New()
			router.Use(middleware.RequestID())
			authorize := middleware.RequireAuthorization(func(_ context.Context, token string) (*domain.Actor, error) {
				if token == "disabled" {
					return nil, domain.ErrAccountDisabled
				}
				return &actor, nil
			}, handler.AuthorizationFailure)
			calls := 0
			confirm := handler.ConfirmSyncChanges(func(callContext context.Context, identity domain.Actor, rawCursor string) (uint64, error) {
				calls++
				if identity != actor || rawCursor != cursor || requestcontext.RequestID(callContext) != requestID {
					t.Fatal("确认服务未收到预期身份、游标和请求 ID")
				}
				if scenario.cancelAfter {
					cancel()
				}
				if scenario.serviceError != nil {
					return scenario.ack, fmt.Errorf("%s: %w", diagnostic, scenario.serviceError)
				}
				return scenario.ack, nil
			})
			router.POST(path, authorize, confirm, func(*gin.Context) { t.Error("响应完成后仍进入下游处理") })
			requestBody := scenario.body
			if requestBody == "" {
				requestBody = body
			}
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(requestBody)).WithContext(ctx)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set(middleware.RequestIDHeader, requestID)
			switch scenario.bearer {
			case "disabled":
				request.Header.Set("Authorization", "Bearer disabled")
			case "":
				request.Header.Set("Authorization", "Bearer fixture-access")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			// 2. 核对响应头、成功数据及失败时的公开边界。
			if calls != scenario.serviceCalls || response.Code != scenario.status ||
				response.Header().Get(middleware.RequestIDHeader) != requestID ||
				response.Header().Get("Cache-Control") != "no-store" || strings.Contains(response.Body.String(), cursor) {
				t.Fatalf("状态、响应头、响应正文或调用次数不符：%d，%v，%s，调用 %d 次", response.Code, response.Header(), response.Body.String(), calls)
			}
			if scenario.status == http.StatusOK {
				var success dto.ConfirmSyncSnapshotBaselineResponse
				if err := json.Unmarshal(response.Body.Bytes(), &success); err != nil || success.RequestID != requestID ||
					success.Data.Status != "ACTIVE" || success.Data.AckSequence != scenario.ackText {
					t.Fatalf("确认成功响应不符：%s，错误 %v", response.Body.String(), err)
				}
				return
			}
			var failure struct {
				dto.ErrorResponse
				Data json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil || failure.RequestID != requestID ||
				failure.Error.Code != scenario.code || len(failure.Data) != 0 || strings.Contains(response.Body.String(), diagnostic) {
				t.Fatalf("确认失败响应不符：%s，错误 %v", response.Body.String(), err)
			}
			if (scenario.field == "" && failure.Error.Details != nil) ||
				(scenario.field != "" && failure.Error.Details["field"] != scenario.field) {
				t.Fatalf("错误字段不符：%+v，期望 %q", failure.Error.Details, scenario.field)
			}
			if scenario.status == http.StatusUnauthorized && response.Header().Get("WWW-Authenticate") == "" {
				t.Fatal("认证失败响应缺少 Bearer 提示")
			}
		})
	}
}
