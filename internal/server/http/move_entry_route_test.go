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

func TestRegisterMoveEntryRoute(t *testing.T) {
	actor := domain.Actor{UserID: 7, DeviceID: 13, SessionID: 17}
	authCalls, moveCalls, detailCalls, listCalls, renameCalls := 0, 0, 0, 0, 0
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
	RegisterRenameEntryRoute(router, authorize, func(context.Context, domain.Actor, uint64, uint64, string, string) (json.RawMessage, error) {
		renameCalls++
		return json.Marshal(renameEntrySuccessFixture(t))
	})
	RegisterMoveEntryRoute(router, authorize, func(ctx context.Context, received domain.Actor, entryID, expectedRevision, targetParentID uint64, key string) (json.RawMessage, error) {
		moveCalls++
		stored, exists := requestcontext.Actor(ctx)
		if received != actor || !exists || stored != actor || requestcontext.RequestID(ctx) == "" || entryID != 23 || expectedRevision != 3 || targetParentID != 14 || key != "Request-007" {
			t.Error("正式移动参数或上下文错误")
		}
		return json.Marshal(moveEntrySuccessFixture(t))
	})
	for _, testCase := range []struct {
		name, method, path                                 string
		bearer, missingKey, preflight, forbidden, rejected bool
		status, auth, moves, details, lists, renames       int
	}{
		{name: "move", method: "POST", path: "/api/v1/entries/23/move", bearer: true, status: 200, auth: 1, moves: 1},
		{name: "missing key", method: "POST", path: "/api/v1/entries/23/move", bearer: true, missingKey: true, status: 400, auth: 1},
		{name: "invalid path first", method: "POST", path: "/api/v1/entries/0/move", bearer: true, missingKey: true, status: 400, auth: 1},
		{name: "authentication first", method: "POST", path: "/api/v1/entries/0/move", missingKey: true, status: 401},
		{name: "rejected token", method: "POST", path: "/api/v1/entries/23/move", bearer: true, rejected: true, status: 401, auth: 1},
		{name: "forbidden origin", method: "POST", path: "/api/v1/entries/23/move", bearer: true, forbidden: true, status: 403},
		{name: "preflight", method: "OPTIONS", path: "/api/v1/entries/23/move", preflight: true, status: 204},
		{name: "detail coexistence", method: "GET", path: "/api/v1/entries/23", bearer: true, status: 200, auth: 1, details: 1},
		{name: "list coexistence", method: "GET", path: "/api/v1/entries?parent_id=12", bearer: true, status: 200, auth: 1, lists: 1},
		{name: "put unsupported", method: "PUT", path: "/api/v1/entries/23/move", status: 404},
		{name: "patch unsupported", method: "PATCH", path: "/api/v1/entries/23/move", status: 404},
		{name: "head unsupported", method: "HEAD", path: "/api/v1/entries/23/move", status: 404},
		{name: "trailing slash", method: "POST", path: "/api/v1/entries/23/move/", status: 404},
		{name: "extra segment", method: "POST", path: "/api/v1/entries/23/move/extra", status: 404},
		{name: "rename coexistence", method: "PATCH", path: "/api/v1/entries/23", bearer: true, status: 200, auth: 1, renames: 1},
		{name: "bare post", method: "POST", path: "/api/v1/entries/23", status: 404},
		{name: "get unsupported", method: "GET", path: "/api/v1/entries/23/move", status: 404},
		{name: "ping", method: "GET", path: "/ping", status: 200},
		{name: "live", method: "GET", path: "/health/live", status: 200},
		{name: "ready", method: "GET", path: "/health/ready", status: 200},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			authCalls, moveCalls, detailCalls, listCalls, renameCalls = 0, 0, 0, 0, 0
			body := `{"target_parent_id":"14","expected_revision":"3"}`
			if testCase.name == "rename coexistence" {
				body = `{"name":"学习笔记.PDF","expected_revision":"3"}`
			}
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
				request.Header.Set("Access-Control-Request-Method", "POST")
				request.Header.Set("Access-Control-Request-Headers", "Authorization, Content-Type, Idempotency-Key")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != testCase.status || authCalls != testCase.auth || moveCalls != testCase.moves || detailCalls != testCase.details || listCalls != testCase.lists || renameCalls != testCase.renames {
				t.Fatal("路由匹配或调用次数错误", response.Code, authCalls, moveCalls, detailCalls, listCalls)
			}
			if testCase.moves == 1 {
				var result dto.MoveEntryResponse
				if json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Data.Entry.ID != "23" || result.Data.Entry.Revision != "4" || result.RequestID != response.Header().Get("X-Request-ID") || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("正式移动响应错误")
				}
			}
			if testCase.preflight && (response.Header().Get("Access-Control-Allow-Origin") != "http://127.0.0.1:5173" || !strings.Contains(response.Header().Get("Access-Control-Allow-Methods"), "POST") || !strings.Contains(response.Header().Get("Access-Control-Allow-Headers"), "Idempotency-Key")) {
				t.Fatal("正式移动预检错误")
			}
		})
	}
}

func TestRegisterMoveEntryRouteDependencies(t *testing.T) {
	for _, missing := range []string{"router", "authorization", "move"} {
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
			move := func(context.Context, domain.Actor, uint64, uint64, uint64, string) (json.RawMessage, error) {
				return nil, nil
			}
			if missing == "authorization" {
				authorize = nil
			}
			if missing == "move" {
				move = nil
			}
			RegisterMoveEntryRoute(router, authorize, move)
		})
	}
}
