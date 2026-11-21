package http

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

func TestRegisterUploadPartURLsRoute(t *testing.T) {
	// 1. 同时装配现有任务查询和批量签发，记录实际调用的依赖。
	const origin = "http://127.0.0.1:5173"
	const taskPath = "/api/v1/uploads/19"
	const route = taskPath + "/part-urls"
	const credential = "fixture-access"
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
	digest := strings.Repeat("0", 64)
	body := fmt.Sprintf(`{"parts":[{"part_number":2,"sha256":%q}]}`, digest)
	expiry := time.Date(2026, 11, 21, 12, 10, 0, 123456000, time.UTC)
	var authCalls, issueCalls, queryCalls int
	var targetID uint64
	authorize := func(_ context.Context, token string) (*domain.Actor, error) {
		authCalls++
		if token != credential {
			return nil, domain.ErrInvalidAccessToken
		}
		return &actor, nil
	}
	router := NewRouter(logging.New(io.Discard), []string{origin}, func(context.Context) bool { return true })
	RegisterUploadTaskRoute(router, authorize, func(context.Context, domain.Actor, uint64) (*domain.UploadTask, error) {
		queryCalls++
		return uploadTaskSuccessFixture(), nil
	})
	RegisterUploadPartURLsRoute(router, authorize, func(ctx context.Context, received domain.Actor, taskID uint64, parts []domain.UploadPartDeclaration, expected *time.Time) (*domain.UploadPartURLBatch, error) {
		issueCalls++
		targetID = taskID
		stored, exists := requestcontext.Actor(ctx)
		if authCalls != 1 || !exists || stored != actor || received != actor || requestcontext.RequestID(ctx) != requestID ||
			len(parts) != 1 || parts[0].PartNumber != 2 || parts[0].SHA256 != digest || expected != nil {
			t.Error("签发依赖未收到认证后的身份、请求 ID 和声明")
		}
		return &domain.UploadPartURLBatch{ExpiresAt: expiry, Parts: []domain.UploadPartURL{
			{PartNumber: 2, SizeBytes: 1, ChecksumSHA256: strings.Repeat("A", 43) + "=",
				URL: "https://objects.example.invalid/fixture?partNumber=2&X-Amz-Signature=fixture", URLExpiresAt: expiry.Add(-time.Minute)},
		}}, nil
	})

	// 2. 验证路径、方法、Bearer 认证和共享 CORS 在同一条路由上的顺序。
	for _, test := range []struct {
		name, method, path, authorization, origin, code string
		status, auth, issues, queries                   int
		taskID                                          uint64
		preflight                                       bool
	}{
		{name: "POST", method: http.MethodPost, path: route, authorization: "Bearer " + credential,
			status: 200, auth: 1, issues: 1, taskID: 19},
		{name: "decoded ID", method: http.MethodPost, path: "/api/v1/uploads/%31%39/part-urls", authorization: "Bearer " + credential,
			status: 200, auth: 1, issues: 1, taskID: 19, origin: origin},
		{name: "maximum ID", method: http.MethodPost, path: "/api/v1/uploads/18446744073709551615/part-urls", authorization: "Bearer " + credential,
			status: 200, auth: 1, issues: 1, taskID: math.MaxUint64},
		{name: "authorization before path", method: http.MethodPost, path: "/api/v1/uploads/0/part-urls", status: 401, code: "AUTH_REQUIRED"},
		{name: "invalid scheme", method: http.MethodPost, path: route, authorization: "Basic fixture", status: 401, code: "AUTH_REQUIRED"},
		{name: "invalid token", method: http.MethodPost, path: route, authorization: "Bearer rejected", status: 401, auth: 1, code: "AUTH_REQUIRED"},
		{name: "path validation after authorization", method: http.MethodPost, path: "/api/v1/uploads/0/part-urls", authorization: "Bearer " + credential,
			status: 400, auth: 1, code: "INVALID_ARGUMENT"},
		{name: "CORS preflight", method: http.MethodOptions, path: route, origin: origin, status: 204, preflight: true},
		{name: "forbidden origin", method: http.MethodPost, path: route, authorization: "Bearer " + credential,
			origin: "https://unlisted.example.invalid", status: 403, code: "CORS_FORBIDDEN"},
		{name: "wrong method", method: http.MethodGet, path: route, authorization: "Bearer " + credential, status: 404, code: "ROUTE_NOT_FOUND"},
		{name: "trailing slash", method: http.MethodPost, path: route + "/", authorization: "Bearer " + credential, status: 404, code: "ROUTE_NOT_FOUND"},
		{name: "task query", method: http.MethodGet, path: taskPath, authorization: "Bearer " + credential, status: 200, auth: 1, queries: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			authCalls, issueCalls, queryCalls, targetID = 0, 0, 0, 0
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Request-ID", requestID)
			if test.authorization != "" {
				request.Header.Set("Authorization", test.authorization)
			}
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			if test.preflight {
				request.Header.Set("Access-Control-Request-Method", http.MethodPost)
				request.Header.Set("Access-Control-Request-Headers", "Authorization, Content-Type, X-Request-ID")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.status || authCalls != test.auth || issueCalls != test.issues || queryCalls != test.queries ||
				targetID != test.taskID || response.Header().Get("X-Request-ID") != requestID {
				t.Fatal("正式签发路径、认证或调用次数有误", response.Code, authCalls, issueCalls, queryCalls, targetID)
			}
			if test.origin == origin && response.Header().Get("Access-Control-Allow-Origin") != origin {
				t.Fatal("允许的来源缺少 CORS 响应头")
			}
			if test.preflight && (response.Body.Len() != 0 || !strings.Contains(response.Header().Get("Access-Control-Allow-Headers"), "Authorization, Content-Type")) {
				t.Fatal("预检未返回允许授权头的空响应")
			}
			if (response.Header().Get("WWW-Authenticate") != "") != (test.status == http.StatusUnauthorized) {
				t.Fatal("认证提示头不符合响应状态")
			}
			if test.issues > 0 {
				var result dto.UploadPartURLsResponse
				if json.Unmarshal(response.Body.Bytes(), &result) != nil || result.RequestID != requestID || !result.Data.ExpiresAt.Equal(expiry) ||
					len(result.Data.Parts) != 1 || result.Data.Parts[0].PartNumber != 2 || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("正式路径未交付签发响应")
				}
			}
			if test.code != "" {
				var failure dto.ErrorResponse
				if json.Unmarshal(response.Body.Bytes(), &failure) != nil || failure.RequestID != requestID || failure.Error.Code != test.code {
					t.Fatal("正式路径错误响应不符", failure.Error.Code)
				}
			}
		})
	}
}

func TestRegisterUploadPartURLsRouteDependencies(t *testing.T) {
	// 1. 任一路由依赖缺失时，在装配阶段直接失败。
	for _, missing := range []string{"router", "authorization", "signer"} {
		t.Run(missing, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("缺少依赖仍完成批量签发路由注册")
				}
			}()
			var router *gin.Engine
			if missing != "router" {
				router = NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			}
			authorize := func(context.Context, string) (*domain.Actor, error) { return nil, nil }
			create := func(context.Context, domain.Actor, uint64, []domain.UploadPartDeclaration, *time.Time) (*domain.UploadPartURLBatch, error) {
				return nil, nil
			}
			if missing == "authorization" {
				authorize = nil
			}
			if missing == "signer" {
				create = nil
			}
			RegisterUploadPartURLsRoute(router, authorize, create)
		})
	}
}
