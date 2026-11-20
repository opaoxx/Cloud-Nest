package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
)

func TestRegisterRestoreEntryRoute(t *testing.T) {
	const origin = "http://127.0.0.1:5173"
	const restorePath = "/api/v1/trash/23/restore"
	actor := domain.Actor{UserID: 7, DeviceID: 19, SessionID: 17}
	for _, scenario := range []struct {
		name, method, path          string
		bearer                      bool
		status, authCalls, restores int
	}{
		{"post", http.MethodPost, restorePath, true, 200, 1, 1},
		{"authorization before path", http.MethodPost, "/api/v1/trash/0/restore", false, 401, 0, 0},
		{"invalid target", http.MethodPost, "/api/v1/trash/0/restore", true, 400, 1, 0},
		{"get", http.MethodGet, restorePath, true, 404, 0, 0},
		{"entries path", http.MethodPost, "/api/v1/entries/23/restore", true, 404, 0, 0},
		{"trailing slash", http.MethodPost, restorePath + "/", true, 404, 0, 0},
		{"preflight", http.MethodOptions, restorePath, false, 204, 0, 0},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 在正式路径注册认证和恢复服务，并检查调用顺序。
			authCalls, restores := 0, 0
			router := NewRouter(logging.New(io.Discard), []string{origin}, func(context.Context) bool { return true })
			RegisterRestoreEntryRoute(router, func(_ context.Context, token string) (*domain.Actor, error) {
				authCalls++
				if token != "fixture-access" {
					t.Fatal("路由未传递当前凭证")
				}
				return &actor, nil
			}, func(_ context.Context, identity domain.Actor, entryID, revision uint64, key string) (json.RawMessage, error) {
				restores++
				if authCalls != 1 || identity != actor || entryID != 23 || revision != 4 || key != "restore-key" {
					t.Fatal("恢复服务收到错误的认证上下文或参数")
				}
				return json.RawMessage(`{"entry":{"id":"23","parent_id":"11","name":"笔记 (1).md","entry_type":"FILE","status":"ACTIVE","revision":"5"}}`), nil
			})

			// 2. 核对方法、路径、认证失败的顺序及浏览器预检。
			request := httptest.NewRequest(scenario.method, scenario.path, strings.NewReader(`{"expected_revision":"4"}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Idempotency-Key", "restore-key")
			if scenario.bearer {
				request.Header.Set("Authorization", "Bearer fixture-access")
			}
			if scenario.method == http.MethodOptions {
				request.Header.Set("Origin", origin)
				request.Header.Set("Access-Control-Request-Method", http.MethodPost)
				request.Header.Set("Access-Control-Request-Headers", "Authorization, Content-Type, Idempotency-Key")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != scenario.status || authCalls != scenario.authCalls || restores != scenario.restores {
				t.Fatal("正式恢复路由状态或调用次数不符", response.Code, authCalls, restores)
			}
			if scenario.method == http.MethodOptions && (response.Header().Get("Access-Control-Allow-Origin") != origin || response.Body.Len() != 0) {
				t.Fatal("正式恢复路径的预检响应不符")
			}
			if response.Code == http.StatusOK {
				var result dto.RestoreEntryResponse
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Data.Entry.Revision != "5" ||
					result.Data.Entry.ParentID != "11" || result.RequestID == "" || result.RequestID != response.Header().Get("X-Request-ID") {
					t.Fatal("正式恢复路由的成功快照不符", result, err)
				}
			}
		})
	}
}
