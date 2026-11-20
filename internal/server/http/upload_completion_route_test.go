package http

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
)

func TestRegisterUploadCompletionRoute(t *testing.T) {
	// 1. 装配真实路由，记录授权和服务调用。
	const origin = "http://127.0.0.1:5173"
	actor := domain.Actor{UserID: 7, SessionID: 17, DeviceID: 11}
	var authorizations, confirmations int
	router := NewRouter(logging.New(io.Discard), []string{origin}, func(context.Context) bool { return true })
	RegisterUploadCompletionRoute(router, func(_ context.Context, token string) (*domain.Actor, error) {
		authorizations++
		if token != "fixture-access" {
			return nil, domain.ErrInvalidAccessToken
		}
		return &actor, nil
	}, func(_ context.Context, received domain.Actor, taskID uint64) (*domain.UploadTask, error) {
		confirmations++
		if received != actor || taskID != math.MaxUint64 {
			t.Error("完成确认未收到授权身份和完整任务 ID", received, taskID)
		}
		task := uploadTaskSuccessFixture()
		task.ID, task.Status = taskID, domain.UploadStatusVerifying
		return task, nil
	})

	// 2. 核对路径、方法、授权顺序和浏览器预检。
	const path = "/api/v1/uploads/18446744073709551615/complete"
	for _, scenario := range []struct {
		name, method, path, token string
		status, auth, confirm     int
	}{
		{"maximum ID", http.MethodPost, path, "fixture-access", 202, 1, 1},
		{"authorization before path", http.MethodPost, "/api/v1/uploads/0/complete", "", 401, 0, 0},
		{"invalid ID", http.MethodPost, "/api/v1/uploads/0/complete", "fixture-access", 400, 1, 0},
		{"invalid token", http.MethodPost, path, "invalid", 401, 1, 0},
		{"preflight", http.MethodOptions, path, "", 204, 0, 0},
		{"only POST", http.MethodGet, path, "fixture-access", 404, 0, 0},
		{"extra slash", http.MethodPost, path + "/", "fixture-access", 404, 0, 0},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			authorizations, confirmations = 0, 0
			request := httptest.NewRequest(scenario.method, scenario.path, nil)
			if scenario.token != "" {
				request.Header.Set("Authorization", "Bearer "+scenario.token)
			}
			if scenario.method == http.MethodOptions {
				request.Header.Set("Origin", origin)
				request.Header.Set("Access-Control-Request-Method", http.MethodPost)
				request.Header.Set("Access-Control-Request-Headers", "Authorization, X-Request-ID")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != scenario.status || authorizations != scenario.auth || confirmations != scenario.confirm {
				t.Fatal("完成确认路由状态或调用次数不符", response.Code, authorizations, confirmations)
			}
			if scenario.method == http.MethodOptions && response.Header().Get("Access-Control-Allow-Origin") != origin {
				t.Fatal("完成确认预检缺少允许来源")
			}
			if scenario.confirm == 1 {
				var body dto.UploadTaskResponse
				if json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Data.ID != "18446744073709551615" ||
					body.Data.Status != "VERIFYING" || body.RequestID == "" || body.RequestID != response.Header().Get("X-Request-ID") {
					t.Fatal("完成确认公开响应错误", body)
				}
			}
		})
	}
}
