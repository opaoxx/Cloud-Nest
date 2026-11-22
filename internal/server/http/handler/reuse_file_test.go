package handler

import (
	"context"
	"crypto/sha256"
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
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

const reuseSuccessJSON = `{"entry":{"id":"42","parent_id":"18446744073709551615","name":"学习笔记 (1).PDF","entry_type":"FILE","size_bytes":0,"revision":"1","created_at":"2026-11-21T08:00:00.123456Z","modified_at":"2026-11-21T08:00:00.123456Z","updated_at":"2026-11-21T08:00:00.123456Z","status":"ACTIVE"}}`
const reuseFixtureRequestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
const reuseFixtureToken = "synthetic-bearer-token"

func reuseHandlerRouter(t *testing.T, actor domain.Actor, handler gin.HandlerFunc) *gin.Engine {
	t.Helper()
	router := gin.New()
	router.Use(middleware.RequestID())
	router.POST("/fixture", middleware.RequireAuthorization(func(_ context.Context, token string) (*domain.Actor, error) {
		if token != reuseFixtureToken {
			return nil, domain.ErrInvalidAccessToken
		}
		return &actor, nil
	}, AuthorizationFailure), handler, func(*gin.Context) { t.Error("秒传处理器完成后仍进入下游") })
	return router
}

func reuseHandlerRequest(ctx context.Context, requestID string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/fixture", strings.NewReader(reuseRequestJSON)).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+reuseFixtureToken)
	request.Header.Set("Idempotency-Key", "Reuse-A")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(middleware.RequestIDHeader, requestID)
	return request
}

func TestReuseFileHandlerSuccess(t *testing.T) {
	// 1. 使用真实授权中间件和合成授权结果，记录服务收到的声明。
	actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
	calls := 0
	router := reuseHandlerRouter(t, actor, ReuseFile(func(ctx context.Context, received domain.Actor, parentID uint64,
		name string, sizeBytes uint64, digest [sha256.Size]byte, key string,
	) (json.RawMessage, error) {
		calls++
		if received != actor || requestcontext.RequestID(ctx) == "" || parentID != math.MaxUint64 || name != "  学习笔记.PDF  " ||
			sizeBytes != 0 || digest != sha256.Sum256(nil) || key != "Reuse-A" {
			t.Fatal("服务未收到认证身份和完整的秒传参数")
		}
		return json.RawMessage(reuseSuccessJSON), nil
	}))

	// 2. 连续交付同一快照，每次使用当前请求 ID，保留原名称和版本。
	for _, requestID := range []string{reuseFixtureRequestID, "38ee758a-0727-42de-ae7d-3bb33155e148"} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, reuseHandlerRequest(t.Context(), requestID))
		wantBody := `{"data":` + reuseSuccessJSON + `,"request_id":"` + requestID + `"}`
		if response.Code != http.StatusCreated || response.Body.String() != wantBody ||
			response.Header().Get(middleware.RequestIDHeader) != requestID ||
			response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" ||
			response.Header().Get("Content-Type") != "application/json; charset=utf-8" {
			t.Fatalf("秒传成功响应不符，得到 %d、%s 和 %v", response.Code, response.Body.String(), response.Header())
		}
	}
	if calls != 2 {
		t.Fatalf("服务调用次数不符，得到 %d", calls)
	}
}

func TestReuseFileHandlerErrors(t *testing.T) {
	const privateText = "synthetic-internal-detail"
	actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
	for _, scenario := range []struct {
		label, code, field, body string
		serviceError             error
		status                   int
	}{
		{label: "invalid name", code: "INVALID_NAME", field: "name", serviceError: domain.ErrInvalidEntryName, status: 400},
		{label: "parent is a file", code: "INVALID_ARGUMENT", field: "parent_id", serviceError: domain.ErrFileEntryNotDirectory, status: 400},
		{label: "depth exceeded", code: "INVALID_ARGUMENT", field: "parent_id", serviceError: domain.ErrFileEntryDepthExceeded, status: 400},
		{label: "parent unavailable", code: "ENTRY_NOT_FOUND", serviceError: domain.ErrFileEntryNotFound, status: 404},
		{label: "content unavailable", code: "REUSABLE_CONTENT_NOT_FOUND", serviceError: domain.ErrReusableContentNotFound, status: 404},
		{label: "idempotency conflict", code: "IDEMPOTENCY_CONFLICT", serviceError: domain.ErrIdempotencyRequestConflict, status: 409},
		{label: "idempotency expired", code: "IDEMPOTENCY_EXPIRED", serviceError: domain.ErrIdempotencyRecordExpired, status: 409},
		{label: "quota exceeded", code: "QUOTA_EXCEEDED", serviceError: domain.ErrQuotaExceeded, status: 507},
		{label: "disabled account", code: "ACCOUNT_DISABLED", serviceError: domain.ErrAccountDisabled, status: 403},
		{label: "session revoked", code: "SESSION_REVOKED", serviceError: domain.ErrSessionRevoked, status: 401},
		{label: "internal failure", code: "INTERNAL_ERROR", serviceError: errors.New("synthetic repository failure"), status: 500},
		{label: "wrong snapshot parent", code: "INTERNAL_ERROR", body: strings.Replace(reuseSuccessJSON, "18446744073709551615", "11", 1), status: 500},
		{label: "wrong snapshot size", code: "INTERNAL_ERROR", body: strings.Replace(reuseSuccessJSON, `"size_bytes":0`, `"size_bytes":1`, 1), status: 500},
		{label: "snapshot contains private field", code: "INTERNAL_ERROR", body: strings.Replace(reuseSuccessJSON, `"id":"42"`, `"id":"42","object_id":"29"`, 1), status: 500},
	} {
		t.Run(scenario.label, func(t *testing.T) {
			// 1. 用已授权请求触发服务失败或快照不一致。
			router := reuseHandlerRouter(t, actor, ReuseFile(func(context.Context, domain.Actor, uint64, string, uint64, [sha256.Size]byte, string) (json.RawMessage, error) {
				if scenario.serviceError != nil {
					return nil, fmt.Errorf("%s: %w", privateText, scenario.serviceError)
				}
				return json.RawMessage(scenario.body), nil
			}))
			response := httptest.NewRecorder()
			router.ServeHTTP(response, reuseHandlerRequest(t.Context(), reuseFixtureRequestID))

			// 2. 核对公开错误、字段定位和无敏感细节的响应。
			var result dto.ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || response.Code != scenario.status ||
				result.Error.Code != scenario.code || result.Error.Message == "" || result.RequestID != reuseFixtureRequestID ||
				strings.Contains(response.Body.String(), privateText) || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("秒传失败响应不符，得到 %d、%s 和 %v", response.Code, response.Body.String(), err)
			}
			if scenario.field == "" && result.Error.Details != nil ||
				scenario.field != "" && (len(result.Error.Details) != 1 || result.Error.Details["field"] != scenario.field) {
				t.Fatalf("公开错误字段不符，得到 %+v", result.Error.Details)
			}
			if scenario.serviceError == domain.ErrReusableContentNotFound && result.Error.Message != "没有可复用的内容" {
				t.Fatalf("内容未命中提示不符，得到 %q", result.Error.Message)
			}
		})
	}
}

func TestReuseFileHandlerRejectsBeforeService(t *testing.T) {
	handler := ReuseFile(func(context.Context, domain.Actor, uint64, string, uint64, [sha256.Size]byte, string) (json.RawMessage, error) {
		t.Fatal("拒绝的 HTTP 请求仍然进入了秒传服务")
		return nil, nil
	})
	actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
	protected := reuseHandlerRouter(t, actor, handler)
	misconfigured := gin.New()
	misconfigured.POST("/fixture", handler)

	// 1. 缺凭证由认证中间件拒绝，缺幂等键由绑定器拒绝。
	for _, scenario := range []struct {
		label, header string
		router        *gin.Engine
		wantStatus    int
	}{
		{"missing bearer", "Authorization", protected, http.StatusUnauthorized},
		{"missing key", "Idempotency-Key", protected, http.StatusBadRequest},
		{"missing trusted context", "", misconfigured, http.StatusInternalServerError},
	} {
		t.Run(scenario.label, func(t *testing.T) {
			request := reuseHandlerRequest(t.Context(), reuseFixtureRequestID)
			request.Header.Del(scenario.header)
			response := httptest.NewRecorder()
			scenario.router.ServeHTTP(response, request)
			if response.Code != scenario.wantStatus {
				t.Fatalf("拒绝状态不符，得到 %d 和 %s", response.Code, response.Body.String())
			}
		})
	}
}
