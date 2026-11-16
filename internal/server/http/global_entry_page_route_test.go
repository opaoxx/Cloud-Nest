package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

func TestRegisterGlobalEntryPageRoute(t *testing.T) {
	for _, testCase := range []struct {
		name, method, path, raw, code, field, search, kind, cursor, scope string
		status, auth, directories, globals, details, size                 int
		parentID                                                          uint64
		bearer, preflight, budget, foreignOrigin                          bool
		authorizationError, serviceError                                  error
	}{
		{name: "starred default", raw: "scope=starred", scope: "starred", bearer: true, status: 200, auth: 1, globals: 1, size: 50},
		{name: "starred filters", raw: "scope=starred&sort=modified_desc&q=+%E5%AD%A6%E4%B9%A0%25_%2B+&kind=document&limit=2", scope: "starred", search: " 学习%_+ ", kind: "document", bearer: true, status: 200, auth: 1, globals: 1, size: 2},
		{name: "encoded starred", raw: "%73cope=%73tarred&limit=1", scope: "starred", bearer: true, status: 200, auth: 1, globals: 1, size: 1},
		{name: "starred ignored parent", raw: "scope=starred&parent_id=private-parent&parent_id=0&user_id=99", scope: "starred", bearer: true, status: 200, auth: 1, globals: 1, size: 50},
		{name: "starred cursor", raw: "scope=starred&cursor=private-starred-cursor&limit=100", scope: "starred", cursor: "private-starred-cursor", bearer: true, status: 200, auth: 1, globals: 1, size: 100},
		{name: "starred budget empty", raw: "scope=starred", scope: "starred", budget: true, bearer: true, status: 200, auth: 1, globals: 1, size: 50},
		{name: "duplicate starred", raw: "scope=starred&%73cope=starred", bearer: true, status: 400, auth: 1, code: "INVALID_ARGUMENT", field: "scope"},
		{name: "mixed global scopes", raw: "scope=all&scope=starred", bearer: true, status: 400, auth: 1, code: "INVALID_ARGUMENT", field: "scope"},
		{name: "starred bad limit", raw: "scope=starred&limit=0", bearer: true, status: 400, auth: 1, code: "INVALID_ARGUMENT", field: "limit"},
		{name: "starred bad search", raw: "scope=starred&q=%FF", bearer: true, status: 400, auth: 1, code: "INVALID_ARGUMENT", field: "q"},
		{name: "starred malformed query", raw: "scope=starred&note=%ZZ", bearer: true, status: 400, auth: 1, code: "INVALID_ARGUMENT", field: "query"},
		{name: "starred missing authorization", raw: "scope=starred&limit=0", status: 401, code: "AUTH_REQUIRED"},
		{name: "starred revoked authorization", raw: "scope=starred&limit=0", bearer: true, authorizationError: domain.ErrSessionRevoked, status: 401, auth: 1, code: "SESSION_REVOKED"},
		{name: "starred invalid cursor", raw: "scope=starred", scope: "starred", bearer: true, serviceError: domain.ErrInvalidGlobalEntryCursor, status: 400, auth: 1, globals: 1, size: 50, code: "INVALID_CURSOR", field: "cursor"},
		{name: "starred internal failure", raw: "scope=starred", scope: "starred", bearer: true, serviceError: errors.New("private-service-error"), status: 500, auth: 1, globals: 1, size: 50, code: "INTERNAL_ERROR"},
		{name: "starred preflight", method: "OPTIONS", raw: "scope=starred", preflight: true, status: 204},
		{name: "default directory", raw: "parent_id=12", bearer: true, status: 200, auth: 1, directories: 1, parentID: 12, size: 50},
		{name: "explicit directory", raw: "scope=directory&parent_id=12&limit=2", bearer: true, status: 200, auth: 1, directories: 1, parentID: 12, size: 2},
		{name: "directory cursor", raw: "parent_id=12&cursor=private-directory-cursor", cursor: "private-directory-cursor", bearer: true, status: 200, auth: 1, directories: 1, parentID: 12, size: 50},
		{name: "all defaults", raw: "scope=all", bearer: true, status: 200, auth: 1, globals: 1, size: 50},
		{name: "all filters", raw: "scope=all&sort=modified_desc&q=+%E5%AD%A6%E4%B9%A0%25_%2B+&kind=document&limit=2", search: " 学习%_+ ", kind: "document", bearer: true, status: 200, auth: 1, globals: 1, size: 2},
		{name: "encoded scope", raw: "%73cope=%61ll&limit=1", bearer: true, status: 200, auth: 1, globals: 1, size: 1},
		{name: "all ignores parent", raw: "scope=all&parent_id=private-parent&parent_id=0&user_id=99", bearer: true, status: 200, auth: 1, globals: 1, size: 50},
		{name: "all cursor", raw: "scope=all&cursor=private-global-cursor&limit=100", cursor: "private-global-cursor", bearer: true, status: 200, auth: 1, globals: 1, size: 100},
		{name: "budget empty", raw: "scope=all", budget: true, bearer: true, status: 200, auth: 1, globals: 1, size: 50},
		{name: "missing scope and parent", bearer: true, status: 400, auth: 1, code: "INVALID_ARGUMENT", field: "parent_id"},
		{name: "empty scope", raw: "scope=&parent_id=12", bearer: true, status: 400, auth: 1, code: "INVALID_ARGUMENT", field: "scope"},
		{name: "unsupported scope", raw: "scope=unknown", bearer: true, status: 400, auth: 1, code: "INVALID_ARGUMENT", field: "scope"},
		{name: "uppercase scope", raw: "scope=ALL", bearer: true, status: 400, auth: 1, code: "INVALID_ARGUMENT", field: "scope"},
		{name: "uppercase key", raw: "SCOPE=all&parent_id=12", bearer: true, status: 200, auth: 1, directories: 1, parentID: 12, size: 50},
		{name: "duplicate all", raw: "scope=all&scope=all", bearer: true, status: 400, auth: 1, code: "INVALID_ARGUMENT", field: "scope"},
		{name: "mixed scope", raw: "scope=directory&%73cope=all&parent_id=12", bearer: true, status: 400, auth: 1, code: "INVALID_ARGUMENT", field: "scope"},
		{name: "invalid complete query", raw: "scope=all&note=%ZZ", bearer: true, status: 400, auth: 1, code: "INVALID_ARGUMENT", field: "query"},
		{name: "semicolon query", raw: "scope=all;limit=1", bearer: true, status: 400, auth: 1, code: "INVALID_ARGUMENT", field: "query"},
		{name: "all invalid sort", raw: "scope=all&sort=size_desc", bearer: true, status: 400, auth: 1, code: "INVALID_ARGUMENT", field: "sort"},
		{name: "all invalid utf8", raw: "scope=all&q=%FF", bearer: true, status: 400, auth: 1, code: "INVALID_ARGUMENT", field: "q"},
		{name: "all duplicate kind", raw: "scope=all&kind=&kind=", bearer: true, status: 400, auth: 1, code: "INVALID_ARGUMENT", field: "kind"},
		{name: "all invalid limit", raw: "scope=all&limit=0", bearer: true, status: 400, auth: 1, code: "INVALID_ARGUMENT", field: "limit"},
		{name: "directory search", raw: "parent_id=12&q=private-search", bearer: true, status: 400, auth: 1, code: "INVALID_ARGUMENT", field: "q"},
		{name: "missing authorization", raw: "scope=all&limit=0", status: 401, code: "AUTH_REQUIRED"},
		{name: "authorization before malformed query", raw: "scope=all&note=%ZZ", status: 401, code: "AUTH_REQUIRED"},
		{name: "revoked authorization", raw: "scope=all&limit=0", bearer: true, authorizationError: domain.ErrSessionRevoked, status: 401, auth: 1, code: "SESSION_REVOKED"},
		{name: "global invalid cursor", raw: "scope=all", bearer: true, serviceError: domain.ErrInvalidGlobalEntryCursor, status: 400, auth: 1, globals: 1, size: 50, code: "INVALID_CURSOR", field: "cursor"},
		{name: "global internal error", raw: "scope=all", bearer: true, serviceError: errors.New("private-service-error"), status: 500, auth: 1, globals: 1, size: 50, code: "INTERNAL_ERROR"},
		{name: "preflight", method: "OPTIONS", raw: "scope=all", preflight: true, status: 204},
		{name: "origin rejected", raw: "scope=all", bearer: true, foreignOrigin: true, status: 403, code: "CORS_FORBIDDEN"},
		{name: "wrong method", method: "POST", raw: "scope=all", bearer: true, status: 404, code: "ROUTE_NOT_FOUND"},
		{name: "detail coexistence", path: "/api/v1/entries/23", raw: "scope=all", bearer: true, status: 200, auth: 1, details: 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 注册同一路径的两类分页，并核对可信身份及准确服务参数。
			actor := domain.Actor{UserID: 7, DeviceID: 19, SessionID: 17}
			authCalls, directoryCalls, globalCalls, detailCalls := 0, 0, 0, 0
			var logs bytes.Buffer
			router := NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
			authorize := func(_ context.Context, token string) (*domain.Actor, error) {
				authCalls++
				if token != "fixture-access" {
					t.Error("正式列表路由未准确传递 Bearer")
				}
				return &actor, testCase.authorizationError
			}
			checkActor := func(ctx context.Context, received domain.Actor) {
				t.Helper()
				stored, exists := requestcontext.Actor(ctx)
				if !exists || stored != actor || received != actor || requestcontext.RequestID(ctx) == "" {
					t.Error("列表路由缺少准确身份或请求 ID")
				}
			}
			RegisterEntryPageRoute(router, authorize,
				func(ctx context.Context, received domain.Actor, parentID uint64, cursor string, size int) (*domain.DirectoryPageResult, error) {
					directoryCalls++
					checkActor(ctx, received)
					if parentID != testCase.parentID || cursor != testCase.cursor || size != testCase.size {
						t.Error("目录分派的参数错误")
					}
					return directoryPageSuccessFixture(), nil
				}, func(ctx context.Context, received domain.Actor, query domain.GlobalEntryQuery, cursor string) (*domain.GlobalEntryPageResult, error) {
					globalCalls++
					checkActor(ctx, received)
					expected := domain.GlobalEntryQuery{Scope: "all", Sort: "modified_desc", Search: testCase.search, Kind: testCase.kind, PageSize: testCase.size}
					if testCase.scope != "" {
						expected.Scope = testCase.scope
					}
					if query != expected || cursor != testCase.cursor {
						t.Error("全局分派的参数错误")
					}
					result := globalEntryPageSuccessFixture()
					if testCase.budget {
						result.Entries = nil
					}
					return result, testCase.serviceError
				}, func(context.Context, domain.Actor, domain.GlobalEntryQuery, string) (*domain.RecentEntryPageResult, error) {
					t.Error("原目录/全局/收藏请求进入最近服务")
					return nil, nil
				})
			RegisterEntryDetailRoute(router, authorize, func(context.Context, domain.Actor, uint64) (*domain.EntryDetail, error) {
				detailCalls++
				return entryDetailSuccessFixture(), nil
			})
			method, path := testCase.method, testCase.path
			if method == "" {
				method = "GET"
			}
			if path == "" {
				path = "/api/v1/entries"
			}
			request := httptest.NewRequest(method, path, nil)
			request.URL.RawQuery = testCase.raw
			request.RequestURI = request.URL.RequestURI()
			request.Header.Set("X-Request-ID", "28EE758A-0727-42DE-AE7D-3BB33155E148")
			if testCase.bearer {
				request.Header.Set("Authorization", "Bearer fixture-access")
			}
			request.Header.Set("Origin", "http://127.0.0.1:5173")
			if testCase.foreignOrigin {
				request.Header.Set("Origin", "https://other.example")
			}
			if testCase.preflight {
				request.Header.Set("Access-Control-Request-Method", "GET")
				request.Header.Set("Access-Control-Request-Headers", "Authorization, X-Request-ID")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			// 2. 检查单次认证、查询分派、响应形状及固定字段错误。
			if response.Code != testCase.status || authCalls != testCase.auth || directoryCalls != testCase.directories || globalCalls != testCase.globals || detailCalls != testCase.details {
				t.Fatal("正式列表的状态或分派次数错误", response.Code, authCalls, directoryCalls, globalCalls, detailCalls)
			}
			if testCase.status == 200 && testCase.details == 0 {
				var body dto.GlobalEntryPageResponse
				if json.Unmarshal(response.Body.Bytes(), &body) != nil || body.RequestID != response.Header().Get("X-Request-ID") || body.Data.Items == nil || body.Data.Breadcrumbs == nil {
					t.Fatal("正式列表成功结构错误")
				}
				if (len(body.Data.Breadcrumbs) == 0) != (testCase.globals == 1) || (len(body.Data.Items) == 0) != testCase.budget || body.Data.NextCursor == nil {
					t.Fatal("目录/全局面包屑、预算空页或游标错误")
				}
			}
			if testCase.code != "" {
				var body dto.ErrorResponse
				if json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Error.Code != testCase.code || body.RequestID != response.Header().Get("X-Request-ID") {
					t.Fatal("正式列表错误结构或业务码错误")
				}
				if testCase.field == "" {
					if body.Error.Details != nil {
						t.Fatal("普通错误 details 非空")
					}
				} else if len(body.Error.Details) != 1 || body.Error.Details["field"] != testCase.field {
					t.Fatal("正式列表错误字段错误")
				}
			}
			if strings.Contains(response.Body.String(), "private-") || strings.Contains(logs.String(), "private-") {
				t.Fatal("正式列表正文或日志包含原始输入")
			}
		})
	}
}

func TestRegisterGlobalEntryPageRouteDependencies(t *testing.T) {
	for _, mode := range []string{"nil global", "multiple globals"} {
		t.Run(mode, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("全局列表依赖错误仍完成注册")
				}
			}()
			router := gin.New()
			authorize := func(context.Context, string) (*domain.Actor, error) { return nil, nil }
			directory := func(context.Context, domain.Actor, uint64, string, int) (*domain.DirectoryPageResult, error) {
				return nil, nil
			}
			global := func(context.Context, domain.Actor, domain.GlobalEntryQuery, string) (*domain.GlobalEntryPageResult, error) {
				return nil, nil
			}
			if mode == "nil global" {
				RegisterDirectoryPageRoute(router, authorize, directory, nil)
				return
			}
			RegisterDirectoryPageRoute(router, authorize, directory, global, global)
		})
	}
}
