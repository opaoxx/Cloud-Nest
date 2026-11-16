package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

func TestRegisterSizeEntryPageRoute(t *testing.T) {
	for _, fixture := range []struct {
		name, raw, service, scope, search, kind, cursor, code, field string
		status, size                                                 int
		missingBearer, revoked, preflight, foreignOrigin, budget     bool
		serviceError                                                 error
	}{
		{name: "all size", raw: "scope=all&sort=size_desc", service: "size", scope: "all", status: 200, size: 50},
		{name: "starred size", raw: "scope=starred&sort=size_desc", service: "size", scope: "starred", status: 200, size: 50},
		{name: "encoded size", raw: "%73cope=%61ll&%73ort=%73ize_desc&limit=1", service: "size", scope: "all", status: 200, size: 1},
		{name: "filters", raw: "scope=all&sort=size_desc&q=" + url.QueryEscape(" 学习%_+ ") + "&kind=document&limit=2", service: "size", scope: "all", search: " 学习%_+ ", kind: "document", status: 200, size: 2},
		{name: "long cursor", raw: "scope=starred&sort=size_desc&cursor=" + strings.Repeat("a", 355), service: "size", scope: "starred", cursor: strings.Repeat("a", 355), status: 200, size: 50},
		{name: "maximum cursor", raw: "scope=all&sort=size_desc&cursor=" + strings.Repeat("a", 1024), service: "size", scope: "all", cursor: strings.Repeat("a", 1024), status: 200, size: 50},
		{name: "budget page", raw: "scope=all&sort=size_desc", service: "size", scope: "all", budget: true, status: 200, size: 50},
		{name: "identity extras", raw: "scope=starred&sort=size_desc&parent_id=private-parent&parent_id=0&user_id=99", service: "size", scope: "starred", status: 200, size: 50},
		{name: "default directory", raw: "parent_id=12", service: "directory", status: 200},
		{name: "explicit directory", raw: "scope=directory&parent_id=12", service: "directory", status: 200},
		{name: "default all", raw: "scope=all", service: "global", status: 200},
		{name: "default starred", raw: "scope=starred", service: "global", status: 200},
		{name: "explicit modified", raw: "scope=starred&sort=modified_desc", service: "global", status: 200},
		{name: "recent", raw: "scope=recent", service: "recent", status: 200},
		{name: "missing scope with size sort", raw: "sort=size_desc", status: 400, code: "INVALID_ARGUMENT", field: "sort"},
		{name: "missing scope and parent", status: 400, code: "INVALID_ARGUMENT", field: "parent_id"},
		{name: "directory size", raw: "parent_id=12&sort=size_desc", status: 400, code: "INVALID_ARGUMENT", field: "sort"},
		{name: "recent size", raw: "scope=recent&sort=size_desc", status: 400, code: "INVALID_ARGUMENT", field: "sort"},
		{name: "duplicate scope", raw: "scope=all&scope=all&sort=size_desc", status: 400, code: "INVALID_ARGUMENT", field: "scope"},
		{name: "mixed scope", raw: "scope=all&%73cope=starred&sort=size_desc", status: 400, code: "INVALID_ARGUMENT", field: "scope"},
		{name: "unknown scope", raw: "scope=private-scope&sort=size_desc", status: 400, code: "INVALID_ARGUMENT", field: "scope"},
		{name: "duplicate sort", raw: "scope=all&sort=size_desc&%73ort=size_desc", status: 400, code: "INVALID_ARGUMENT", field: "sort"},
		{name: "mixed sort", raw: "scope=all&sort=size_desc&sort=modified_desc", status: 400, code: "INVALID_ARGUMENT", field: "sort"},
		{name: "unknown sort", raw: "scope=all&sort=private-sort", status: 400, code: "INVALID_ARGUMENT", field: "sort"},
		{name: "name sort", raw: "scope=all&sort=name_asc", service: "name", status: 200},
		{name: "uppercase sort", raw: "scope=all&sort=SIZE_DESC", status: 400, code: "INVALID_ARGUMENT", field: "sort"},
		{name: "double encoded sort", raw: "scope=all&sort=%2573ize_desc", status: 400, code: "INVALID_ARGUMENT", field: "sort"},
		{name: "invalid search", raw: "scope=all&sort=size_desc&q=%FF", status: 400, code: "INVALID_ARGUMENT", field: "q"},
		{name: "invalid kind", raw: "scope=starred&sort=size_desc&kind=private-kind", status: 400, code: "INVALID_ARGUMENT", field: "kind"},
		{name: "invalid limit", raw: "scope=all&sort=size_desc&limit=0", status: 400, code: "INVALID_ARGUMENT", field: "limit"},
		{name: "duplicate cursor", raw: "scope=all&sort=size_desc&cursor=&cursor=", status: 400, code: "INVALID_ARGUMENT", field: "cursor"},
		{name: "oversized cursor", raw: "scope=all&sort=size_desc&cursor=" + strings.Repeat("a", 1025), status: 400, code: "INVALID_ARGUMENT", field: "cursor"},
		{name: "malformed query", raw: "scope=all&sort=size_desc&note=%ZZ", status: 400, code: "INVALID_ARGUMENT", field: "query"},
		{name: "missing bearer", raw: "scope=all&sort=size_desc&limit=0", missingBearer: true, status: 401, code: "AUTH_REQUIRED"},
		{name: "authorization before malformed", raw: "scope=all&sort=size_desc&note=%ZZ", missingBearer: true, status: 401, code: "AUTH_REQUIRED"},
		{name: "revoked authorization", raw: "scope=all&sort=size_desc&limit=0", revoked: true, status: 401, code: "SESSION_REVOKED"},
		{name: "cursor error", raw: "scope=all&sort=size_desc", service: "size", scope: "all", status: 400, size: 50, serviceError: domain.ErrInvalidSizeEntryCursor, code: "INVALID_CURSOR", field: "cursor"},
		{name: "service error", raw: "scope=starred&sort=size_desc", service: "size", scope: "starred", status: 500, size: 50, serviceError: errors.New("private-service-error"), code: "INTERNAL_ERROR"},
		{name: "preflight", raw: "scope=all&sort=size_desc", preflight: true, status: 204},
		{name: "foreign origin", raw: "scope=all&sort=size_desc", foreignOrigin: true, status: 403, code: "CORS_FORBIDDEN"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 注册五种正式分页能力，核对认证顺序和准确的大小参数。
			actor := domain.Actor{UserID: 7, SessionID: 17, DeviceID: 19}
			var calls []string
			var logs bytes.Buffer
			router := NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
			checkActor := func(ctx context.Context, identity domain.Actor) {
				t.Helper()
				stored, exists := requestcontext.Actor(ctx)
				if !exists || stored != actor || identity != actor || requestcontext.RequestID(ctx) == "" {
					t.Error("正式大小路由身份或请求关联错误")
				}
			}
			RegisterSizeEntryPageRoute(router, func(_ context.Context, token string) (*domain.Actor, error) {
				calls = append(calls, "auth")
				if token != "fixture-access" {
					t.Error("认证参数错误")
				}
				if fixture.revoked {
					return nil, domain.ErrSessionRevoked
				}
				return &actor, nil
			}, func(ctx context.Context, identity domain.Actor, parentID uint64, cursor string, size int) (*domain.DirectoryPageResult, error) {
				calls = append(calls, "directory")
				checkActor(ctx, identity)
				if parentID != 12 || cursor != "" || size != 50 {
					t.Error("目录分派参数错误")
				}
				return directoryPageSuccessFixture(), nil
			}, func(ctx context.Context, identity domain.Actor, query domain.GlobalEntryQuery, cursor string) (*domain.GlobalEntryPageResult, error) {
				calls = append(calls, "global")
				checkActor(ctx, identity)
				if query.Sort != "modified_desc" || cursor != "" {
					t.Error("修改时间分派参数错误")
				}
				return globalEntryPageSuccessFixture(), nil
			}, func(ctx context.Context, identity domain.Actor, query domain.GlobalEntryQuery, cursor string) (*domain.RecentEntryPageResult, error) {
				calls = append(calls, "recent")
				checkActor(ctx, identity)
				if query.Scope != "recent" || query.Sort != "last_opened_desc" || cursor != "" {
					t.Error("最近分派参数错误")
				}
				return recentEntryPageSuccessFixture(), nil
			}, func(ctx context.Context, identity domain.Actor, query domain.GlobalEntryQuery, cursor string) (*domain.NameEntryPageResult, error) {
				calls = append(calls, "name")
				checkActor(ctx, identity)
				if query.Scope != "all" || query.Sort != "name_asc" || cursor != "" {
					t.Error("名称分派参数错误")
				}
				return nameEntryPageSuccessFixture(), nil
			}, func(ctx context.Context, identity domain.Actor, query domain.GlobalEntryQuery, cursor string) (*domain.SizeEntryPageResult, error) {
				calls = append(calls, "size")
				checkActor(ctx, identity)
				expected := domain.GlobalEntryQuery{Scope: fixture.scope, Sort: "size_desc", Search: fixture.search, Kind: fixture.kind, PageSize: fixture.size}
				if query != expected || cursor != fixture.cursor {
					t.Error("大小分派参数错误")
				}
				result := sizeEntryPageSuccessFixture()
				if fixture.budget {
					result.Entries = nil
				}
				return result, fixture.serviceError
			})
			method := "GET"
			if fixture.preflight {
				method = "OPTIONS"
			}
			request := httptest.NewRequest(method, "/api/v1/entries", nil)
			request.URL.RawQuery = fixture.raw
			request.RequestURI = request.URL.RequestURI()
			request.Header.Set("Origin", "http://127.0.0.1:5173")
			if !fixture.missingBearer {
				request.Header.Set("Authorization", "Bearer fixture-access")
			}
			if fixture.preflight {
				request.Header.Set("Access-Control-Request-Method", "GET")
				request.Header.Set("Access-Control-Request-Headers", "Authorization")
			}
			if fixture.foreignOrigin {
				request.Header.Set("Origin", "https://other.example")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			expectedCalls := []string(nil)
			if !fixture.preflight && !fixture.foreignOrigin && !fixture.missingBearer {
				expectedCalls = append(expectedCalls, "auth")
			}
			if fixture.service != "" {
				expectedCalls = append(expectedCalls, fixture.service)
			}
			if response.Code != fixture.status || !reflect.DeepEqual(calls, expectedCalls) {
				t.Fatal("正式大小分派状态或顺序错误", response.Code, calls)
			}
			if fixture.status == 200 {
				var body dto.SizeEntryPageResponse
				if fixture.service == "size" && (json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Data.Items == nil || len(body.Data.Breadcrumbs) != 0 || (fixture.budget && (len(body.Data.Items) != 0 || body.Data.NextCursor == nil))) {
					t.Fatal("正式大小响应或预算空页错误")
				}
			} else if fixture.status != 204 {
				var body dto.ErrorResponse
				if json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Error.Code != fixture.code || (fixture.field != "" && body.Error.Details["field"] != fixture.field) {
					t.Fatal("正式大小错误响应错误", body.Error.Code, body.Error.Details)
				}
			}
			if strings.Contains(response.Body.String(), "private-") || strings.Contains(logs.String(), "private-") {
				t.Fatal("正式大小响应或日志包含原文")
			}
		})
	}
}

func TestRegisterSizeEntryPageRouteDependencies(t *testing.T) {
	for _, missing := range []string{"router", "authorization", "directory", "global", "recent", "name", "size"} {
		t.Run(missing, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("缺少大小路由依赖仍注册成功")
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
			recent := func(context.Context, domain.Actor, domain.GlobalEntryQuery, string) (*domain.RecentEntryPageResult, error) {
				return nil, nil
			}
			name := func(context.Context, domain.Actor, domain.GlobalEntryQuery, string) (*domain.NameEntryPageResult, error) {
				return nil, nil
			}
			size := func(context.Context, domain.Actor, domain.GlobalEntryQuery, string) (*domain.SizeEntryPageResult, error) {
				return nil, nil
			}
			switch missing {
			case "router":
				router = nil
			case "authorization":
				authorize = nil
			case "directory":
				directory = nil
			case "global":
				global = nil
			case "recent":
				recent = nil
			case "name":
				name = nil
			case "size":
				size = nil
			}
			RegisterSizeEntryPageRoute(router, authorize, directory, global, recent, name, size)
		})
	}
}
