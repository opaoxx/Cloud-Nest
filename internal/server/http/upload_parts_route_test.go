package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

func TestRegisterUploadPartsRoute(t *testing.T) {
	const origin = "http://127.0.0.1:5173"
	const taskPath = "/api/v1/uploads/19"
	const partsPath = taskPath + "/parts"
	const credential = "fixture-access"
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	const checksum = "TgdAhWK+24tgzgXB3s/jrRa3IjCWfeAfZAt+Rym0n84="
	const partSize = 16 << 20

	for _, test := range []struct {
		name, method, path, token, origin, errorCode string
		status, authCalls                            int
		endpoint                                     string
	}{
		{name: "receipts", method: http.MethodGet, path: partsPath, token: credential, origin: origin,
			status: http.StatusOK, authCalls: 1, endpoint: "receipts"},
		{name: "decoded task ID", method: http.MethodGet, path: "/api/v1/uploads/%31%39/parts", token: credential,
			status: http.StatusOK, authCalls: 1, endpoint: "receipts"},
		{name: "authentication precedes path validation", method: http.MethodGet, path: "/api/v1/uploads/0/parts",
			status: http.StatusUnauthorized, errorCode: "AUTH_REQUIRED"},
		{name: "invalid token", method: http.MethodGet, path: partsPath, token: "rejected",
			status: http.StatusUnauthorized, authCalls: 1, errorCode: "AUTH_REQUIRED"},
		{name: "invalid task ID", method: http.MethodGet, path: "/api/v1/uploads/0/parts", token: credential,
			status: http.StatusBadRequest, authCalls: 1, errorCode: "INVALID_ARGUMENT"},
		{name: "CORS preflight", method: http.MethodOptions, path: partsPath, origin: origin, status: http.StatusNoContent},
		{name: "blocked origin", method: http.MethodGet, path: partsPath, token: credential, origin: "https://unlisted.example.invalid",
			status: http.StatusForbidden, errorCode: "CORS_FORBIDDEN"},
		{name: "wrong method", method: http.MethodPost, path: partsPath, token: credential,
			status: http.StatusNotFound, errorCode: "ROUTE_NOT_FOUND"},
		{name: "trailing slash", method: http.MethodGet, path: partsPath + "/", token: credential,
			status: http.StatusNotFound, errorCode: "ROUTE_NOT_FOUND"},
		{name: "task query", method: http.MethodGet, path: taskPath, token: credential,
			status: http.StatusOK, authCalls: 1, endpoint: "task"},
		{name: "part signing", method: http.MethodPost, path: taskPath + "/part-urls", token: credential,
			status: http.StatusOK, authCalls: 1, endpoint: "signing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			// 1. 将三条上传路由挂到同一个 Gin 路由器，记录认证和业务调用。
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			authCalls := 0
			var calledEndpoints []string
			authorize := func(_ context.Context, token string) (*domain.Actor, error) {
				authCalls++
				if token != credential {
					return nil, domain.ErrInvalidAccessToken
				}
				return &actor, nil
			}
			record := func(ctx context.Context, received domain.Actor, taskID uint64, endpoint string) {
				t.Helper()
				calledEndpoints = append(calledEndpoints, endpoint)
				stored, exists := requestcontext.Actor(ctx)
				if authCalls != 1 || !exists || received != actor || stored != actor ||
					taskID != 19 || requestcontext.RequestID(ctx) != requestID {
					t.Error("路由未传递认证身份、任务 ID 或请求 ID")
				}
			}
			router := NewRouter(logging.New(io.Discard), []string{origin}, func(context.Context) bool { return true })
			RegisterUploadTaskRoute(router, authorize, func(ctx context.Context, received domain.Actor, taskID uint64) (*domain.UploadTask, error) {
				record(ctx, received, taskID, "task")
				return uploadTaskSuccessFixture(), nil
			})
			RegisterUploadPartURLsRoute(router, authorize, func(ctx context.Context, received domain.Actor, taskID uint64, _ []domain.UploadPartDeclaration, _ *time.Time) (*domain.UploadPartURLBatch, error) {
				record(ctx, received, taskID, "signing")
				expiry := time.Date(2026, 11, 21, 12, 10, 0, 0, time.UTC)
				return &domain.UploadPartURLBatch{ExpiresAt: expiry, Parts: []domain.UploadPartURL{
					{PartNumber: 2, SizeBytes: 1, ChecksumSHA256: strings.Repeat("A", 43) + "=",
						URL: "https://objects.example.invalid/fixture?partNumber=2&X-Amz-Signature=fixture", URLExpiresAt: expiry.Add(-time.Minute)},
				}}, nil
			})
			RegisterUploadPartsRoute(router, authorize, func(ctx context.Context, received domain.Actor, taskID uint64) (*domain.UploadPartReceiptList, error) {
				record(ctx, received, taskID, "receipts")
				return &domain.UploadPartReceiptList{PartSizeBytes: partSize, PartCount: 2, Parts: []domain.UploadPartReceipt{
					{PartNumber: 2, SizeBytes: 1, ETag: "Tail", ChecksumSHA256: checksum},
				}}, nil
			})

			// 2. 发送合成请求，核对路径、方法、认证及跨域行为。
			var body io.Reader
			if test.endpoint == "signing" {
				body = strings.NewReader(`{"parts":[{"part_number":2,"sha256":"` + strings.Repeat("0", 64) + `"}]}`)
			}
			request := httptest.NewRequest(test.method, test.path, body)
			if body != nil {
				request.Header.Set("Content-Type", "application/json")
			}
			request.Header.Set("X-Request-ID", requestID)
			if test.token != "" {
				request.Header.Set("Authorization", "Bearer "+test.token)
			}
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			if test.method == http.MethodOptions {
				request.Header.Set("Access-Control-Request-Method", http.MethodGet)
				request.Header.Set("Access-Control-Request-Headers", "Authorization")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.status || authCalls != test.authCalls || response.Header().Get("X-Request-ID") != requestID {
				t.Fatalf("状态、认证调用次数或请求 ID 有误：%d, %d, %s", response.Code, authCalls, response.Body.String())
			}
			if test.endpoint == "" && len(calledEndpoints) != 0 ||
				test.endpoint != "" && (len(calledEndpoints) != 1 || calledEndpoints[0] != test.endpoint) {
				t.Fatalf("调用的业务处理器有误：%v", calledEndpoints)
			}
			if test.origin == origin && response.Header().Get("Access-Control-Allow-Origin") != origin {
				t.Fatal("允许的来源缺少 CORS 响应头")
			}
			if test.method == http.MethodOptions && (response.Body.Len() != 0 ||
				!strings.Contains(response.Header().Get("Access-Control-Allow-Headers"), "Authorization")) {
				t.Fatal("预检未返回允许授权头的空响应")
			}
			if test.errorCode != "" {
				var failure dto.ErrorResponse
				if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil || failure.Error.Code != test.errorCode ||
					failure.RequestID != requestID {
					t.Fatalf("错误响应有误：%s", response.Body.String())
				}
			}
			if test.endpoint == "receipts" {
				var result dto.UploadPartsResponse
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.RequestID != requestID ||
					result.Data.PartSizeBytes != partSize || result.Data.PartCount != 2 || len(result.Data.Parts) != 1 ||
					result.Data.Parts[0].PartNumber != 2 || result.Data.Parts[0].ETag != "Tail" ||
					response.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("回执响应有误：%s", response.Body.String())
				}
			}
		})
	}
}
