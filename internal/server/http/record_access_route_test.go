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

func TestRegisterRecordAccessRoute(t *testing.T) {
	expectedData, err := dto.NewRecordAccessData(recordAccessSuccessFixture())
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Actor{UserID: 7, DeviceID: 19, SessionID: 17}
	authCalls, accessCalls, detailCalls, listCalls, moveCalls, starCalls := 0, 0, 0, 0, 0, 0
	expectedID, expectedPurpose := uint64(23), "preview"
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
	RegisterMoveEntryRoute(router, authorize, func(context.Context, domain.Actor, uint64, uint64, uint64, string) (json.RawMessage, error) {
		moveCalls++
		return json.Marshal(moveEntrySuccessFixture(t))
	})
	RegisterSetStarredRoute(router, authorize, func(_ context.Context, _ domain.Actor, entryID uint64, _ bool) (*domain.EntryPreference, error) {
		starCalls++
		result := setStarredSuccessFixture()
		result.EntryID = entryID
		return result, nil
	})
	RegisterRecordAccessRoute(router, authorize, func(ctx context.Context, received domain.Actor, entryID uint64, purpose string) (*domain.EntryPreference, error) {
		accessCalls++
		stored, exists := requestcontext.Actor(ctx)
		if received != actor || !exists || stored != actor || requestcontext.RequestID(ctx) == "" || entryID != expectedID || purpose != expectedPurpose {
			t.Error("正式访问参数或上下文错误")
		}
		result := recordAccessSuccessFixture()
		result.EntryID = entryID
		if entryID == 11 {
			return nil, domain.ErrFileEntryNotFile
		}
		return result, nil
	})
	for _, testCase := range []struct {
		name, method, path, body                             string
		bearer, preflight, forbidden, rejected               bool
		status, auth, accesses, details, lists, moves, stars int
		entryID                                              uint64
	}{
		{name: "preview", method: "POST", path: "/api/v1/entries/23/access", bearer: true, status: 200, auth: 1, accesses: 1},
		{name: "open", method: "POST", path: "/api/v1/entries/23/access", body: `{"purpose":"open"}`, bearer: true, status: 200, auth: 1, accesses: 1},
		{name: "directory", method: "POST", path: "/api/v1/entries/11/access", entryID: 11, bearer: true, status: 400, auth: 1, accesses: 1},
		{name: "maximum target", method: "POST", path: "/api/v1/entries/18446744073709551615/access", entryID: ^uint64(0), bearer: true, status: 200, auth: 1, accesses: 1},
		{name: "null purpose", method: "POST", path: "/api/v1/entries/23/access", body: `{"purpose":null}`, bearer: true, status: 400, auth: 1},
		{name: "missing purpose", method: "POST", path: "/api/v1/entries/23/access", body: `{}`, bearer: true, status: 400, auth: 1},
		{name: "download purpose", method: "POST", path: "/api/v1/entries/23/access", body: `{"purpose":"download"}`, bearer: true, status: 400, auth: 1},
		{name: "invalid path first", method: "POST", path: "/api/v1/entries/0/access", body: `{"private-broken":`, bearer: true, status: 400, auth: 1},
		{name: "authentication first", method: "POST", path: "/api/v1/entries/0/access", body: `{"private-broken":`, status: 401},
		{name: "rejected token", method: "POST", path: "/api/v1/entries/23/access", bearer: true, rejected: true, status: 401, auth: 1},
		{name: "forbidden origin", method: "POST", path: "/api/v1/entries/23/access", bearer: true, forbidden: true, status: 403},
		{name: "preflight", method: "OPTIONS", path: "/api/v1/entries/23/access", preflight: true, status: 204},
		{name: "detail coexistence", method: "GET", path: "/api/v1/entries/23", bearer: true, status: 200, auth: 1, details: 1},
		{name: "list coexistence", method: "GET", path: "/api/v1/entries?parent_id=12", bearer: true, status: 200, auth: 1, lists: 1},
		{name: "move coexistence", method: "POST", path: "/api/v1/entries/23/move", body: `{"target_parent_id":"14","expected_revision":"3"}`, bearer: true, status: 200, auth: 1, moves: 1},
		{name: "star coexistence", method: "PUT", path: "/api/v1/entries/23/star", body: `{"starred":true}`, bearer: true, status: 200, auth: 1, stars: 1},
		{name: "get unsupported", method: "GET", path: "/api/v1/entries/23/access", status: 404},
		{name: "put unsupported", method: "PUT", path: "/api/v1/entries/23/access", status: 404},
		{name: "patch unsupported", method: "PATCH", path: "/api/v1/entries/23/access", status: 404},
		{name: "head unsupported", method: "HEAD", path: "/api/v1/entries/23/access", status: 404},
		{name: "trailing slash", method: "POST", path: "/api/v1/entries/23/access/", status: 404},
		{name: "extra segment", method: "POST", path: "/api/v1/entries/23/access/extra", status: 404},
		{name: "bare post", method: "POST", path: "/api/v1/entries/23", status: 404},
		{name: "ping", method: "GET", path: "/ping", status: 200},
		{name: "live", method: "GET", path: "/health/live", status: 200},
		{name: "ready", method: "GET", path: "/health/ready", status: 200},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 请求正式路径，核对共享认证及各接口的调用次数。
			authCalls, accessCalls, detailCalls, listCalls, moveCalls, starCalls = 0, 0, 0, 0, 0, 0
			expectedID, expectedPurpose = testCase.entryID, "preview"
			if testCase.name == "open" {
				expectedPurpose = "open"
			}
			if expectedID == 0 {
				expectedID = 23
			}
			body := testCase.body
			if body == "" {
				body = `{"purpose":"preview"}`
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
			if testCase.moves == 1 {
				request.Header.Set("Idempotency-Key", "Request-007")
			}
			if testCase.forbidden {
				request.Header.Set("Origin", "http://private-origin.example")
			}
			if testCase.preflight {
				request.Header.Set("Origin", "http://127.0.0.1:5173")
				request.Header.Set("Access-Control-Request-Method", "POST")
				request.Header.Set("Access-Control-Request-Headers", "Authorization, Content-Type")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != testCase.status || authCalls != testCase.auth || accessCalls != testCase.accesses || detailCalls != testCase.details || listCalls != testCase.lists || moveCalls != testCase.moves || starCalls != testCase.stars {
				t.Fatal("路由匹配或调用次数错误", response.Code, authCalls, accessCalls, detailCalls, listCalls, moveCalls)
			}
			if testCase.accesses == 1 && testCase.status == 200 {
				var result dto.RecordAccessResponse
				if json.Unmarshal(response.Body.Bytes(), &result) != nil || !result.Data.LastOpenedAt.Equal(expectedData.LastOpenedAt) || result.RequestID != response.Header().Get("X-Request-ID") || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("正式访问响应错误")
				}
			}
			if testCase.preflight && (response.Header().Get("Access-Control-Allow-Origin") != "http://127.0.0.1:5173" || !strings.Contains(response.Header().Get("Access-Control-Allow-Methods"), "POST") || !strings.Contains(response.Header().Get("Access-Control-Allow-Headers"), "Authorization")) {
				t.Fatal("正式访问预检错误")
			}
		})
	}
}

func TestRegisterRecordAccessRouteDependencies(t *testing.T) {
	for _, missing := range []string{"router", "authorization", "record access"} {
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
			recordAccess := func(context.Context, domain.Actor, uint64, string) (*domain.EntryPreference, error) { return nil, nil }
			if missing == "authorization" {
				authorize = nil
			}
			if missing == "record access" {
				recordAccess = nil
			}
			RegisterRecordAccessRoute(router, authorize, recordAccess)
		})
	}
}
