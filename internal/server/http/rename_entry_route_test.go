package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

func TestRegisterRenameEntryRoute(t *testing.T) {
	actor := domain.Actor{UserID: 7, DeviceID: 13, SessionID: 17}
	authCalls, renameCalls, detailCalls, listCalls := 0, 0, 0, 0
	authorize := func(_ context.Context, token string) (*domain.Actor, error) {
		authCalls++
		if token != "fixture-access" {
			return nil, domain.ErrInvalidAccessToken
		}
		return &actor, nil
	}
	router := NewRouter(logging.New(io.Discard), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
	RegisterEntryDetailRoute(router, authorize, func(context.Context, domain.Actor, uint64) (*domain.EntryDetail, error) {
		detailCalls++
		return entryDetailSuccessFixture(), nil
	})
	RegisterDirectoryPageRoute(router, authorize, func(context.Context, domain.Actor, uint64, string, int) (*domain.DirectoryPageResult, error) {
		listCalls++
		return directoryPageSuccessFixture(), nil
	})
	RegisterRenameEntryRoute(router, authorize, func(ctx context.Context, received domain.Actor, entryID, expectedRevision uint64, name, key string) (json.RawMessage, error) {
		renameCalls++
		stored, exists := requestcontext.Actor(ctx)
		if received != actor || !exists || stored != actor || requestcontext.RequestID(ctx) == "" || entryID != 23 || expectedRevision != 3 || name != "学习笔记.PDF" || key != "Request-007" {
			t.Error("正式重命名参数或上下文错误")
		}
		return json.Marshal(renameEntrySuccessFixture(t))
	})
	for _, testCase := range []struct {
		name, method, path                                 string
		bearer, missingKey, preflight, forbidden, rejected bool
		status, auth, renames, details, lists              int
	}{
		{name: "rename", method: "PATCH", path: "/api/v1/entries/23", bearer: true, status: 200, auth: 1, renames: 1},
		{name: "missing key", method: "PATCH", path: "/api/v1/entries/23", bearer: true, missingKey: true, status: 400, auth: 1},
		{name: "invalid path first", method: "PATCH", path: "/api/v1/entries/0", bearer: true, missingKey: true, status: 400, auth: 1},
		{name: "authentication first", method: "PATCH", path: "/api/v1/entries/0", missingKey: true, status: 401},
		{name: "rejected token", method: "PATCH", path: "/api/v1/entries/23", bearer: true, rejected: true, status: 401, auth: 1},
		{name: "forbidden origin", method: "PATCH", path: "/api/v1/entries/23", bearer: true, forbidden: true, status: 403},
		{name: "preflight", method: "OPTIONS", path: "/api/v1/entries/23", preflight: true, status: 204},
		{name: "detail coexistence", method: "GET", path: "/api/v1/entries/23", bearer: true, status: 200, auth: 1, details: 1},
		{name: "list coexistence", method: "GET", path: "/api/v1/entries?parent_id=12", bearer: true, status: 200, auth: 1, lists: 1},
		{name: "put unsupported", method: "PUT", path: "/api/v1/entries/23", status: 404},
		{name: "post unsupported", method: "POST", path: "/api/v1/entries/23", status: 404},
		{name: "head unsupported", method: "HEAD", path: "/api/v1/entries/23", status: 404},
		{name: "trailing slash", method: "PATCH", path: "/api/v1/entries/23/", status: 404},
		{name: "extra segment", method: "PATCH", path: "/api/v1/entries/23/extra", status: 404},
		{name: "ping", method: "GET", path: "/ping", status: 200},
		{name: "live", method: "GET", path: "/health/live", status: 200},
		{name: "ready", method: "GET", path: "/health/ready", status: 200},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			authCalls, renameCalls, detailCalls, listCalls = 0, 0, 0, 0
			body := `{"name":"学习笔记.PDF","expected_revision":"3"}`
			if testCase.missingKey {
				body = `{"private-broken":`
			}
			request := httptest.NewRequest(testCase.method, testCase.path, strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			if testCase.bearer {
				token := "fixture-access"
				if testCase.rejected {
					token = "rejected-access"
				}
				request.Header.Set("Authorization", "Bearer "+token)
			}
			if !testCase.missingKey {
				request.Header.Set("Idempotency-Key", "Request-007")
			}
			if testCase.forbidden {
				request.Header.Set("Origin", "http://private-origin.example")
			}
			if testCase.preflight {
				request.Header.Set("Origin", "http://127.0.0.1:5173")
				request.Header.Set("Access-Control-Request-Method", "PATCH")
				request.Header.Set("Access-Control-Request-Headers", "Authorization, Content-Type, Idempotency-Key")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != testCase.status || authCalls != testCase.auth || renameCalls != testCase.renames || detailCalls != testCase.details || listCalls != testCase.lists {
				t.Fatal("路由匹配或调用次数错误", response.Code, authCalls, renameCalls, detailCalls, listCalls)
			}
			if testCase.renames == 1 {
				var result dto.RenameEntryResponse
				if json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Data.Entry.ID != "23" || result.Data.Entry.Revision != "4" || result.RequestID != response.Header().Get("X-Request-ID") || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("正式重命名响应错误")
				}
			}
			if testCase.preflight && (response.Header().Get("Access-Control-Allow-Origin") != "http://127.0.0.1:5173" || !strings.Contains(response.Header().Get("Access-Control-Allow-Methods"), "PATCH") || !strings.Contains(response.Header().Get("Access-Control-Allow-Headers"), "Idempotency-Key")) {
				t.Fatal("正式重命名预检错误")
			}
		})
	}
}

func TestRegisterRenameEntryRouteDependencies(t *testing.T) {
	for _, missing := range []string{"router", "authorization", "rename"} {
		t.Run(missing, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("缺失依赖仍完成路由注册")
				}
			}()
			var router *gin.Engine
			if missing != "router" {
				router = NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			}
			authorize := func(context.Context, string) (*domain.Actor, error) { return nil, nil }
			rename := func(context.Context, domain.Actor, uint64, uint64, string, string) (json.RawMessage, error) {
				return nil, nil
			}
			if missing == "authorization" {
				authorize = nil
			}
			if missing == "rename" {
				rename = nil
			}
			RegisterRenameEntryRoute(router, authorize, rename)
		})
	}
}
