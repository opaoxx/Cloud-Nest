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

func TestRegisterPurgePreviewRoute(t *testing.T) {
	const origin = "http://127.0.0.1:5173"
	const entryIDText = "18446744073709551615"
	const previewPath = "/api/v1/trash/" + entryIDText + "/purge-preview"
	actor := domain.Actor{UserID: 7, DeviceID: 19, SessionID: 17}
	for _, scenario := range []struct {
		name, method, path          string
		bearer                      bool
		status, authCalls, previews int
	}{
		{"get", http.MethodGet, previewPath, true, 200, 1, 1},
		{"authorization before path", http.MethodGet, "/api/v1/trash/0/purge-preview", false, 401, 0, 0},
		{"invalid target", http.MethodGet, "/api/v1/trash/0/purge-preview", true, 400, 1, 0},
		{"post", http.MethodPost, previewPath, true, 404, 0, 0},
		{"delete", http.MethodDelete, previewPath, true, 404, 0, 0},
		{"entries path", http.MethodGet, "/api/v1/entries/23/purge-preview", true, 404, 0, 0},
		{"trailing slash", http.MethodGet, previewPath + "/", true, 404, 0, 0},
		{"browser preflight", http.MethodOptions, previewPath, false, 204, 0, 0},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 使用正式注册函数，核对认证先于服务调用。
			authCalls, previews := 0, 0
			result := purgePreviewSuccessFixture(t)
			router := NewRouter(logging.New(io.Discard), []string{origin}, func(context.Context) bool { return true })
			RegisterPurgePreviewRoute(router, func(_ context.Context, token string) (*domain.Actor, error) {
				authCalls++
				if token != "fixture-access" {
					t.Fatal("路由未传递当前 Bearer 凭证")
				}
				return &actor, nil
			}, func(_ context.Context, identity domain.Actor, entryID uint64) (*domain.PurgePreview, error) {
				previews++
				if authCalls != 1 || identity != actor || entryID != result.EntryID {
					t.Fatal("预检调用顺序、认证身份或目标错误")
				}
				return result, nil
			})

			// 2. 核对方法、路径和跨域预检。
			request := httptest.NewRequest(scenario.method, scenario.path, nil)
			if scenario.bearer {
				request.Header.Set("Authorization", "Bearer fixture-access")
			}
			if scenario.method == http.MethodOptions {
				request.Header.Set("Origin", origin)
				request.Header.Set("Access-Control-Request-Method", http.MethodGet)
				request.Header.Set("Access-Control-Request-Headers", "Authorization")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != scenario.status || authCalls != scenario.authCalls || previews != scenario.previews {
				t.Fatal("正式预检路由状态或调用次数不符", response.Code, authCalls, previews)
			}
			if scenario.method == http.MethodOptions && (response.Header().Get("Access-Control-Allow-Origin") != origin || response.Body.Len() != 0) {
				t.Fatal("正式预检路径的跨域响应不符")
			}
			if response.Code == http.StatusOK {
				var body dto.PurgePreviewResponse
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Data.EntryID != entryIDText ||
					body.Data.PreviewDigest != result.Credential.PreviewDigest ||
					body.RequestID == "" || body.RequestID != response.Header().Get("X-Request-ID") {
					t.Fatal("正式路由预检响应不符", err)
				}
			}
		})
	}
}
