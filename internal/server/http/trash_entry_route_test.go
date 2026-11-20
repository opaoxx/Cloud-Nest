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

func TestRegisterTrashEntryRoute(t *testing.T) {
	const origin = "http://127.0.0.1:5173"
	const path = "/api/v1/entries/23/trash"
	actor := domain.Actor{UserID: 7, DeviceID: 19, SessionID: 17}
	for _, scenario := range []struct {
		name, method, path   string
		bearer               bool
		status, auth, writes int
	}{
		{"post", http.MethodPost, path, true, 200, 1, 1},
		{"authentication before path", http.MethodPost, "/api/v1/entries/0/trash", false, 401, 0, 0},
		{"invalid path", http.MethodPost, "/api/v1/entries/0/trash", true, 400, 1, 0},
		{"get", http.MethodGet, path, true, 404, 0, 0},
		{"trailing slash", http.MethodPost, path + "/", true, 404, 0, 0},
		{"preflight", http.MethodOptions, path, false, 204, 0, 0},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 将可计数的认证和回收方法装入正式路由。
			authCalls, writes := 0, 0
			router := NewRouter(logging.New(io.Discard), []string{origin}, func(context.Context) bool { return true })
			RegisterTrashEntryRoute(router, func(_ context.Context, token string) (*domain.Actor, error) {
				authCalls++
				if token != "fixture-access" {
					t.Error("路由未传递 Bearer 凭证")
				}
				return &actor, nil
			}, func(_ context.Context, identity domain.Actor, id, revision uint64, key string) (json.RawMessage, error) {
				writes++
				if identity != actor || id != 23 || revision != 3 || key != "trash-key" || authCalls != 1 {
					t.Error("回收服务未取得共享认证或输入参数")
				}
				return json.RawMessage(`{"entry":{"id":"23","parent_id":"12","name":"笔记.md","entry_type":"FILE","status":"TRASHED","revision":"4","deleted_at":"2026-11-20T04:30:00Z"}}`), nil
			})

			// 2. 核对方法、请求路径、认证顺序及浏览器预检。
			request := httptest.NewRequest(scenario.method, scenario.path, strings.NewReader(`{"expected_revision":"3"}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Idempotency-Key", "trash-key")
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
			if response.Code != scenario.status || authCalls != scenario.auth || writes != scenario.writes {
				t.Fatal("正式回收路径的状态或调用次数不符", response.Code, authCalls, writes)
			}
			if scenario.method == http.MethodOptions && (response.Header().Get("Access-Control-Allow-Origin") != origin || response.Body.Len() != 0) {
				t.Fatal("回收预检响应不符")
			}
			if response.Code == http.StatusOK {
				var result dto.TrashEntryResponse
				if json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Data.Entry.Revision != "4" ||
					result.RequestID == "" || result.RequestID != response.Header().Get("X-Request-ID") {
					t.Fatal("正式回收响应不符")
				}
			}
		})
	}
}
