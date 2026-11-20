package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
)

func TestRegisterTrashPageRoute(t *testing.T) {
	const origin = "http://127.0.0.1:5173"
	const path = "/api/v1/trash"
	actor := domain.Actor{UserID: 7, DeviceID: 19, SessionID: 17}
	for _, testCase := range []struct {
		name, method, target   string
		bearer                 bool
		status, auth, searches int
	}{
		{"registered GET", http.MethodGet, path + "?limit=2", true, 200, 1, 1},
		{"auth before query", http.MethodGet, path + "?limit=0", false, 401, 0, 0},
		{"invalid limit", http.MethodGet, path + "?limit=0", true, 400, 1, 0},
		{"POST", http.MethodPost, path, true, 404, 0, 0},
		{"trailing slash", http.MethodGet, path + "/", true, 404, 0, 0},
		{"browser preflight", http.MethodOptions, path, false, 204, 0, 0},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 用可计数的认证与列表方法检查正式路由装配。
			authCalls, listCalls := 0, 0
			router := NewRouter(logging.New(io.Discard), []string{origin}, func(context.Context) bool { return true })
			RegisterTrashPageRoute(router, func(_ context.Context, token string) (*domain.Actor, error) {
				authCalls++
				if token != "fixture-access" {
					t.Error("认证未收到 Bearer 凭证")
				}
				return &actor, nil
			}, func(_ context.Context, identity domain.Actor, pageSize int, cursor string) (*domain.TrashEntryPageResult, error) {
				listCalls++
				if identity != actor || pageSize != 2 || cursor != "" || authCalls != 1 {
					t.Error("正式路由未传递已认证身份和分页参数")
				}
				return &domain.TrashEntryPageResult{}, nil
			})

			// 2. 验证路径、请求方法、认证顺序及预检行为。
			request := httptest.NewRequest(testCase.method, testCase.target, nil)
			if testCase.bearer {
				request.Header.Set("Authorization", "Bearer fixture-access")
			}
			if testCase.method == http.MethodOptions {
				request.Header.Set("Origin", origin)
				request.Header.Set("Access-Control-Request-Method", http.MethodGet)
				request.Header.Set("Access-Control-Request-Headers", "Authorization")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != testCase.status || authCalls != testCase.auth || listCalls != testCase.searches {
				t.Fatalf("状态 %d，认证 %d 次，分页 %d 次", response.Code, authCalls, listCalls)
			}
			if testCase.method == http.MethodOptions && (response.Header().Get("Access-Control-Allow-Origin") != origin || response.Body.Len() != 0) {
				t.Fatal("回收站 GET 的浏览器预检响应不符")
			}
			if response.Code == http.StatusOK {
				var result dto.TrashPageResponse
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Data.Items == nil ||
					len(result.Data.Items) != 0 || result.Data.NextCursor != nil || result.RequestID == "" ||
					result.RequestID != response.Header().Get("X-Request-ID") {
					t.Fatalf("正式回收站响应为 %s，错误为 %v", response.Body.String(), err)
				}
			}
		})
	}
}
