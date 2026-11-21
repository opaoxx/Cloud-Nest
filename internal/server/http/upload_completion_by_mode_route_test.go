package http

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

func TestRegisterUploadCompletionByModeRoute(t *testing.T) {
	const origin = "http://127.0.0.1:5173"
	const taskPath = "/api/v1/uploads/19"
	const completePath = taskPath + "/complete"
	const credential = "fixture-access"
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	mpCalls := []string{"complete lookup", "multipart"}
	for _, test := range []struct {
		name, method, path, token, origin, body, code string
		status, auth                                  int
		taskID                                        uint64
		calls                                         []string
		stage                                         domain.UploadStatus
	}{
		{name: "multipart completion", method: http.MethodPost, path: completePath, token: credential, origin: origin,
			body: `{"parts":[{"part_number":1,"etag":"First"}]}`, status: 202, auth: 1, calls: mpCalls, stage: domain.UploadStatusCompleting},
		{name: "single maximum ID", method: http.MethodPost, path: "/api/v1/uploads/18446744073709551615/complete", token: credential,
			status: 202, auth: 1, taskID: math.MaxUint64, calls: []string{"complete lookup"}, stage: domain.UploadStatusVerifying},
		{name: "task query", method: http.MethodGet, path: taskPath, token: credential,
			status: 200, auth: 1, calls: []string{"query"}, stage: domain.UploadStatusUploading},
		{name: "parts query", method: http.MethodGet, path: taskPath + "/parts", token: credential,
			status: 200, auth: 1, calls: []string{"parts"}},
		{name: "cancel", method: http.MethodPost, path: taskPath + "/cancel", token: credential,
			status: 202, auth: 1, calls: []string{"cancel"}, stage: domain.UploadStatusCancelling},
		{name: "authorization before path", method: http.MethodPost, path: "/api/v1/uploads/0/complete",
			status: 401, code: "AUTH_REQUIRED"},
		{name: "invalid token", method: http.MethodPost, path: completePath, token: "rejected", status: 401, auth: 1, code: "AUTH_REQUIRED"},
		{name: "invalid ID after authorization", method: http.MethodPost, path: "/api/v1/uploads/0/complete", token: credential,
			status: 400, auth: 1, code: "INVALID_ARGUMENT"},
		{name: "preflight", method: http.MethodOptions, path: completePath, origin: origin, status: 204},
		{name: "forbidden origin", method: http.MethodPost, path: completePath, token: credential, origin: "https://unlisted.example.invalid",
			status: 403, code: "CORS_FORBIDDEN"},
		{name: "only POST", method: http.MethodGet, path: completePath, token: credential, status: 404, code: "ROUTE_NOT_FOUND"},
		{name: "trailing slash", method: http.MethodPost, path: completePath + "/", token: credential, status: 404, code: "ROUTE_NOT_FOUND"},
	} {
		t.Run(test.name, func(t *testing.T) {
			// 1. 将完成和相邻路由装在同一 Gin 路由器，记录实际调用链。
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			taskID := test.taskID
			if taskID == 0 {
				taskID = 19
			}
			authCalls := 0
			var calls []string
			authorize := func(_ context.Context, token string) (*domain.Actor, error) {
				authCalls++
				if token != credential {
					return nil, domain.ErrInvalidAccessToken
				}
				return &actor, nil
			}
			record := func(ctx context.Context, received domain.Actor, id uint64, endpoint string) {
				t.Helper()
				calls = append(calls, endpoint)
				stored, exists := requestcontext.Actor(ctx)
				if authCalls != 1 || !exists || stored != actor || received != actor || id != taskID || requestcontext.RequestID(ctx) != requestID {
					t.Fatal("业务调用未收到可信身份、请求 ID 和路径任务 ID")
				}
			}
			router := NewRouter(logging.New(io.Discard), []string{origin}, func(context.Context) bool { return true })
			RegisterUploadCompletionByModeRoute(router, authorize,
				func(ctx context.Context, received domain.Actor, id uint64) (*domain.UploadTask, error) {
					record(ctx, received, id, "complete lookup")
					task := uploadTaskSuccessFixture()
					task.ID = id
					if id == math.MaxUint64 {
						task.Status = domain.UploadStatusVerifying
					} else {
						task.UploadMode = "MULTIPART"
					}
					return task, nil
				}, func(ctx context.Context, received domain.Actor, id uint64, readClaims func() ([]domain.UploadPartCompletionClaim, error)) (*domain.UploadTask, error) {
					record(ctx, received, id, "multipart")
					claims, err := readClaims()
					if err != nil || !slices.Equal(claims, []domain.UploadPartCompletionClaim{{PartNumber: 1, ETag: "First"}}) {
						t.Fatal("正式完成路径未传递回执", claims, err)
					}
					task := uploadTaskSuccessFixture()
					task.ID, task.UploadMode, task.Status = id, "MULTIPART", domain.UploadStatusCompleting
					task.LeaseOwner, task.LeaseUntil = nil, nil
					return task, nil
				})
			RegisterUploadTaskRoute(router, authorize, func(ctx context.Context, received domain.Actor, id uint64) (*domain.UploadTask, error) {
				record(ctx, received, id, "query")
				return uploadTaskSuccessFixture(), nil
			})
			RegisterUploadPartsRoute(router, authorize, func(ctx context.Context, received domain.Actor, id uint64) (*domain.UploadPartReceiptList, error) {
				record(ctx, received, id, "parts")
				return &domain.UploadPartReceiptList{PartSizeBytes: 16 << 20, PartCount: 2}, nil
			})
			RegisterUploadCancellationRoute(router, authorize, func(ctx context.Context, received domain.Actor, id uint64) (*domain.UploadTask, error) {
				record(ctx, received, id, "cancel")
				task := uploadTaskSuccessFixture()
				task.Status, task.LeaseOwner, task.LeaseUntil = domain.UploadStatusCancelling, nil, nil
				return task, nil
			})

			// 2. 请求正式路径，核对状态、授权顺序和路由隔离。
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			request.Header.Set("X-Request-ID", requestID)
			if test.body != "" {
				request.Header.Set("Content-Type", "application/json")
			}
			if test.token != "" {
				request.Header.Set("Authorization", "Bearer "+test.token)
			}
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			if test.method == http.MethodOptions {
				request.Header.Set("Access-Control-Request-Method", http.MethodPost)
				request.Header.Set("Access-Control-Request-Headers", "Authorization, Content-Type")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.status || authCalls != test.auth || !slices.Equal(calls, test.calls) ||
				response.Header().Get("X-Request-ID") != requestID {
				t.Fatal("路由状态或调用链不符", response.Code, authCalls, calls, response.Body.String())
			}
			if test.origin == origin && response.Header().Get("Access-Control-Allow-Origin") != origin {
				t.Fatal("允许来源缺少跨域响应头")
			}
			if test.method == http.MethodOptions && (response.Body.Len() != 0 ||
				!strings.Contains(response.Header().Get("Access-Control-Allow-Headers"), "Authorization")) {
				t.Fatal("预检未返回允许认证头的空响应")
			}
			if test.code != "" {
				var failure dto.ErrorResponse
				if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil || failure.Error.Code != test.code {
					t.Fatal("错误业务码不符", response.Body.String(), err)
				}
			}
			if test.stage != "" {
				var result dto.UploadTaskResponse
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.RequestID != requestID ||
					result.Data.ID != strconv.FormatUint(taskID, 10) || result.Data.Status != string(test.stage) {
					t.Fatal("任务响应错误", response.Body.String(), err)
				}
			}
		})
	}
}
