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

func TestRegisterCreateDownloadRoute(t *testing.T) {
	const origin = "http://127.0.0.1:5173"
	const path = "/api/v1/entries/23/download"
	const token = "fixture-access"
	actor := domain.Actor{UserID: 7, DeviceID: 19, SessionID: 17}
	for _, scenario := range []struct {
		name, method, path           string
		bearer, preflight            bool
		status, authCalls, downloads int
	}{
		{"download", http.MethodPost, path, true, false, 200, 1, 1},
		{"authentication before path", http.MethodPost, "/api/v1/entries/0/download", false, false, 401, 0, 0},
		{"invalid path after authentication", http.MethodPost, "/api/v1/entries/0/download", true, false, 400, 1, 0},
		{"wrong method", http.MethodGet, path, true, false, 404, 0, 0},
		{"trailing slash", http.MethodPost, path + "/", true, false, 404, 0, 0},
		{"preflight", http.MethodOptions, path, false, true, 204, 0, 0},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 给真实路由提供可计数的授权与下载依赖。
			authCalls, downloads := 0, 0
			router := NewRouter(logging.New(io.Discard), []string{origin}, func(context.Context) bool { return true })
			RegisterCreateDownloadRoute(router, func(_ context.Context, receivedToken string) (*domain.Actor, error) {
				authCalls++
				if receivedToken != token {
					t.Error("共享授权收到了错误令牌")
				}
				return &actor, nil
			}, func(_ context.Context, receivedActor domain.Actor, entryID, revision uint64, purpose string) (*domain.EntryDownloadAuthorization, error) {
				downloads++
				if receivedActor != actor || entryID != 23 || revision != 3 || purpose != domain.DownloadPurposeDownload || authCalls != 1 {
					t.Error("下载依赖未收到认证身份和请求参数")
				}
				result := createDownloadSuccessFixture()
				result.Revision = revision
				return result, nil
			})

			// 2. 发送请求，核对方法、路径、认证优先级与预检。
			request := httptest.NewRequest(scenario.method, scenario.path, strings.NewReader(`{"expected_revision":"3","purpose":"download"}`))
			request.Header.Set("Content-Type", "application/json")
			if scenario.bearer {
				request.Header.Set("Authorization", "Bearer "+token)
			}
			if scenario.preflight {
				request.Header.Set("Origin", origin)
				request.Header.Set("Access-Control-Request-Method", "POST")
				request.Header.Set("Access-Control-Request-Headers", "Authorization, Content-Type")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != scenario.status || authCalls != scenario.authCalls || downloads != scenario.downloads {
				t.Fatalf("路由结果不符，状态 %d，授权调用 %d，下载调用 %d", response.Code, authCalls, downloads)
			}
			if scenario.preflight && (response.Header().Get("Access-Control-Allow-Origin") != origin || response.Body.Len() != 0) {
				t.Fatal("正式下载路径的跨域预检不符")
			}
			if response.Code == http.StatusOK {
				var result dto.CreateDownloadResponse
				if json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Data.Revision != "3" ||
					result.RequestID == "" || result.RequestID != response.Header().Get("X-Request-ID") {
					t.Fatal("正式路由未交付有效授权响应")
				}
			}
		})
	}
}
