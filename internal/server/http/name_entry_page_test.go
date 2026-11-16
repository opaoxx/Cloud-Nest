package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

func TestNameEntryPageHandlerFlow(t *testing.T) {
	for _, testCase := range []struct {
		name, raw, code, field, search, kind, cursor, scope string
		status, calls, size, cancelOn                       int
		serviceError                                        error
		change                                              func(*domain.NameEntryPageResult)
		missingActor, nilResult, maximumActor               bool
		cancelBefore, cancelAfter, deadline, panicService   bool
		pending, noOrigin, forbiddenOrigin                  bool
	}{
		{name: "starred query", raw: "scope=starred", scope: "starred", status: 200, calls: 1},
		{name: "starred search query", raw: "scope=starred&q=" + url.QueryEscape(" 学习%_+ ") + "&kind=document&limit=2", scope: "starred", search: " 学习%_+ ", kind: "document", size: 2, status: 200, calls: 1},
		{name: "starred next query", raw: "scope=starred&cursor=private-opaque-cursor&limit=2", scope: "starred", cursor: "private-opaque-cursor", size: 2, status: 200, calls: 1},
		{name: "defaults", status: 200, calls: 1},
		{name: "explicit mode", raw: "scope=all&sort=name_asc&q=&kind=&cursor=", status: 200, calls: 1},
		{name: "search and kind", raw: "q=" + url.QueryEscape(" 学习%_+ ") + "&kind=document&limit=2", search: " 学习%_+ ", kind: "document", size: 2, status: 200, calls: 1},
		{name: "next page", raw: "cursor=private-opaque-cursor&limit=2", cursor: "private-opaque-cursor", size: 2, status: 200, calls: 1},
		{name: "encoded input", raw: "%71=%E5%AD%A6%E4%B9%A0&%6Bind=folder&%6Cimit=%31", search: "学习", kind: "folder", size: 1, status: 200, calls: 1},
		{name: "no origin", noOrigin: true, status: 200, calls: 1},
		{name: "pending success", pending: true, status: 200, calls: 1},
		{name: "budget empty page", status: 200, calls: 1, change: func(result *domain.NameEntryPageResult) { result.Entries = nil }},
		{name: "empty end", status: 200, calls: 1, change: func(result *domain.NameEntryPageResult) { result.Entries, result.NextCursor = nil, nil }},
		{name: "last page", status: 200, calls: 1, change: func(result *domain.NameEntryPageResult) { result.NextCursor = nil }},
		{name: "maximum identity", raw: "limit=100", size: 100, maximumActor: true, status: 200, calls: 1},
		{name: "long input cursor", raw: "cursor=" + strings.Repeat("a", 1691), cursor: strings.Repeat("a", 1691), status: 200, calls: 1},
		{name: "maximum input cursor", raw: "cursor=" + strings.Repeat("a", 2048), cursor: strings.Repeat("a", 2048), status: 200, calls: 1},
		{name: "directory result", status: 200, calls: 1, change: func(result *domain.NameEntryPageResult) {
			result.Entries[0].Entry.EntryType, result.Entries[0].Entry.FileSize = "DIR", 0
		}},
		{name: "long next cursor", status: 200, calls: 1, change: func(result *domain.NameEntryPageResult) { *result.NextCursor = strings.Repeat("b", 1691) }},
		{name: "maximum next cursor", status: 200, calls: 1, change: func(result *domain.NameEntryPageResult) { *result.NextCursor = strings.Repeat("b", 2048) }},
		{name: "missing actor before query", raw: "limit=0", missingActor: true, status: 500, code: "INTERNAL_ERROR"},
		{name: "unsupported scope", raw: "scope=recent", status: 400, code: "INVALID_ARGUMENT", field: "scope"},
		{name: "unsupported sort", raw: "sort=modified_desc", status: 400, code: "INVALID_ARGUMENT", field: "sort"},
		{name: "invalid search", raw: "q=%FF", status: 400, code: "INVALID_ARGUMENT", field: "q"},
		{name: "duplicate search", raw: "q=private-one&%71=private-two", status: 400, code: "INVALID_ARGUMENT", field: "q"},
		{name: "invalid kind", raw: "kind=private-kind", status: 400, code: "INVALID_ARGUMENT", field: "kind"},
		{name: "duplicate cursor", raw: "cursor=a&cursor=b", status: 400, code: "INVALID_ARGUMENT", field: "cursor"},
		{name: "oversized cursor", raw: "cursor=" + strings.Repeat("a", 2049), status: 400, code: "INVALID_ARGUMENT", field: "cursor"},
		{name: "bad limit", raw: "limit=0", status: 400, code: "INVALID_ARGUMENT", field: "limit"},
		{name: "malformed query", raw: "extra=%ZZ", status: 400, code: "INVALID_ARGUMENT", field: "query"},
		{name: "forbidden origin", forbiddenOrigin: true, status: 403, code: "CORS_FORBIDDEN"},
		{name: "invalid cursor", serviceError: domain.ErrInvalidNameEntryCursor, status: 400, calls: 1, code: "INVALID_CURSOR", field: "cursor"},
		{name: "wrapped invalid cursor", serviceError: fmt.Errorf("private-cursor-error: %w", domain.ErrInvalidNameEntryCursor), status: 400, calls: 1, code: "INVALID_CURSOR", field: "cursor"},
		{name: "disabled account", serviceError: domain.ErrAccountDisabled, status: 403, calls: 1, code: "ACCOUNT_DISABLED"},
		{name: "revoked session", serviceError: domain.ErrSessionRevoked, status: 401, calls: 1, code: "SESSION_REVOKED"},
		{name: "missing session", serviceError: domain.ErrSessionNotFound, status: 401, calls: 1, code: "SESSION_REVOKED"},
		{name: "expired session", serviceError: domain.ErrSessionExpired, status: 401, calls: 1, code: "SESSION_REVOKED"},
		{name: "revoked device", serviceError: domain.ErrDeviceRevoked, status: 401, calls: 1, code: "SESSION_REVOKED"},
		{name: "missing account", serviceError: domain.ErrAccountNotFound, status: 401, calls: 1, code: "AUTH_REQUIRED"},
		{name: "data failure", serviceError: domain.ErrInvalidFileEntryData, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "database failure", serviceError: errors.New("private-database-failure"), status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "nil result", nilResult: true, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "foreign result", status: 500, calls: 1, code: "INTERNAL_ERROR", change: func(result *domain.NameEntryPageResult) { result.Entries[0].Entry.UserID = 8 }},
		{name: "foreign later item", status: 500, calls: 1, code: "INTERNAL_ERROR", change: func(result *domain.NameEntryPageResult) {
			other := result.Entries[0]
			other.Entry.ID, other.Entry.UserID = 24, 8
			result.Entries = append(result.Entries, other)
		}},
		{name: "more than requested", raw: "limit=1", size: 1, status: 500, calls: 1, code: "INTERNAL_ERROR", change: func(result *domain.NameEntryPageResult) {
			other := result.Entries[0]
			other.Entry.ID = 24
			result.Entries = append(result.Entries, other)
		}},
		{name: "unsafe bytes", status: 500, calls: 1, code: "INTERNAL_ERROR", change: func(result *domain.NameEntryPageResult) { result.Entries[0].Entry.FileSize = 1 << 53 }},
		{name: "invalid time", status: 500, calls: 1, code: "INTERNAL_ERROR", change: func(result *domain.NameEntryPageResult) { result.Entries[0].Entry.CreatedAt = time.Time{} }},
		{name: "empty next cursor", status: 500, calls: 1, code: "INTERNAL_ERROR", change: func(result *domain.NameEntryPageResult) { *result.NextCursor = "" }},
		{name: "oversized next cursor", status: 500, calls: 1, code: "INTERNAL_ERROR", change: func(result *domain.NameEntryPageResult) { *result.NextCursor = strings.Repeat("b", 2049) }},
		{name: "pending query error", raw: "limit=0", pending: true, status: 400, code: "INVALID_ARGUMENT", field: "limit"},
		{name: "pending cursor error", pending: true, serviceError: domain.ErrInvalidNameEntryCursor, status: 400, calls: 1, code: "INVALID_CURSOR", field: "cursor"},
		{name: "pending auth error", pending: true, serviceError: domain.ErrSessionRevoked, status: 401, calls: 1, code: "SESSION_REVOKED"},
		{name: "cancelled before entry", cancelBefore: true, status: 500, code: "INTERNAL_ERROR"},
		{name: "cancelled before service", cancelOn: 2, status: 500, code: "INTERNAL_ERROR"},
		{name: "cancelled after service", cancelAfter: true, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "cancelled with cursor error", cancelAfter: true, serviceError: domain.ErrInvalidNameEntryCursor, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "cancelled at delivery", cancelOn: 4, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "cancelled before write", cancelOn: 5, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "service deadline", deadline: true, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "service panic", panicService: true, status: 500, calls: 1, code: "INTERNAL_ERROR"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 装配可信 Actor 与服务替身，核对上下文和查询输入。
			actor := domain.Actor{UserID: 7, SessionID: 17, DeviceID: 19}
			result := nameEntryPageSuccessFixture()
			if testCase.maximumActor {
				maximum := ^uint64(0)
				parentID := maximum - 1
				actor = domain.Actor{UserID: maximum, SessionID: maximum, DeviceID: maximum}
				result.Entries[0].Entry.ID, result.Entries[0].Entry.UserID, result.Entries[0].Entry.ParentID = maximum, maximum, &parentID
			}
			if testCase.change != nil {
				testCase.change(result)
			}
			original, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if testCase.cancelBefore {
				cancel()
			}
			if testCase.deadline {
				var release context.CancelFunc
				ctx, release = context.WithTimeout(ctx, 20*time.Millisecond)
				defer release()
			}
			size := testCase.size
			if size == 0 {
				size = 50
			}
			var expectedContext context.Context
			var logs bytes.Buffer
			calls, downstream := 0, 0
			router := NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
			list := handler.NameEntryPage(func(received context.Context, identity domain.Actor, query domain.GlobalEntryQuery, cursor string) (*domain.NameEntryPageResult, error) {
				calls++
				expected := domain.GlobalEntryQuery{Scope: "all", Sort: "name_asc", Search: testCase.search, Kind: testCase.kind, PageSize: size}
				if testCase.scope != "" {
					expected.Scope = testCase.scope
				}
				if received != expectedContext || identity != actor || query != expected || cursor != testCase.cursor {
					t.Error("分页服务收到的身份、上下文或查询参数错误")
				}
				if testCase.deadline {
					<-received.Done()
					return nil, received.Err()
				}
				if testCase.cancelAfter {
					cancel()
				}
				if testCase.panicService {
					panic("private-service-panic")
				}
				if testCase.nilResult {
					return nil, nil
				}
				return result, testCase.serviceError
			})
			router.GET("/test-name-entry-page", func(requestContext *gin.Context) {
				if !testCase.missingActor {
					requestContext.Request = requestContext.Request.WithContext(requestcontext.WithActor(requestContext.Request.Context(), actor))
				}
				if testCase.cancelOn != 0 {
					requestContext.Request = requestContext.Request.WithContext(&cancelUserSessionsContext{Context: requestContext.Request.Context(), cancel: cancel, cancelOn: testCase.cancelOn})
				}
				if testCase.pending {
					requestContext.Header("Content-Length", "3")
					requestContext.Header("Content-Encoding", "gzip")
					requestContext.Header("Content-Type", "application/octet-stream")
				}
				expectedContext = requestContext.Request.Context()
			}, list, func(*gin.Context) { downstream++ })
			request := httptest.NewRequest("GET", "/test-name-entry-page", strings.NewReader(`{"user_id":"99"}`)).WithContext(ctx)
			request.URL.RawQuery = testCase.raw + "&user_id=99&parent_id=private-parent&note=private-query"
			request.RequestURI = request.URL.RequestURI()
			request.Header.Set("Authorization", "Bearer private-access")
			request.Header.Set("X-User-ID", "99")
			request.Header.Set("X-Request-ID", "28EE758A-0727-42DE-AE7D-3BB33155E148")
			request.AddCookie(&http.Cookie{Name: handler.RefreshCookieName, Value: "private-refresh"})
			if !testCase.noOrigin {
				request.Header.Set("Origin", "http://127.0.0.1:5173")
			}
			if testCase.forbiddenOrigin {
				request.Header.Set("Origin", "https://other.example")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			// 2. 核对成功数据、固定错误、响应头及处理链结束。
			if response.Code != testCase.status || calls != testCase.calls || downstream != 0 || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Content-Type") != "application/json; charset=utf-8" || response.Header().Get("Content-Encoding") != "" || len(response.Header().Values("Set-Cookie")) != 0 {
				t.Fatal("状态、调用顺序或响应头错误", response.Code, calls, downstream)
			}
			if response.Header().Get("X-Request-ID") != "28ee758a-0727-42de-ae7d-3bb33155e148" || (testCase.code != "CORS_FORBIDDEN" && response.Header().Get("Pragma") != "no-cache") {
				t.Fatal("请求 ID 或禁止缓存头错误")
			}
			if testCase.status == 200 {
				var body dto.NameEntryPageResponse
				expected, err := dto.NewNameEntryPageData(result)
				if err != nil || json.Unmarshal(response.Body.Bytes(), &body) != nil || !reflect.DeepEqual(body.Data, *expected) || body.RequestID != response.Header().Get("X-Request-ID") || response.Header().Get("Content-Length") != strconv.Itoa(response.Body.Len()) {
					t.Fatal("名称成功 JSON、空页或游标交付错误", err)
				}
			} else {
				var body dto.ErrorResponse
				if json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Error.Code != testCase.code || body.RequestID != response.Header().Get("X-Request-ID") {
					t.Fatal("统一错误结构或业务码错误")
				}
				if testCase.field == "" {
					if body.Error.Details != nil {
						t.Fatal("非字段错误 details 非空")
					}
				} else if len(body.Error.Details) != 1 || body.Error.Details["field"] != testCase.field {
					t.Fatal("字段错误位置错误")
				}
				if testCase.code == "INVALID_CURSOR" && body.Error.Message != "名称分页游标无效，请重新查询" {
					t.Fatal("无效游标公开提示错误")
				}
			}
			if (testCase.status == 401) != (response.Header().Get("WWW-Authenticate") != "") {
				t.Fatal("认证提示与状态不一致")
			}

			// 3. 核对来源结果，检查正文与日志的原文保护。
			after, err := json.Marshal(result)
			if err != nil || !bytes.Equal(original, after) || strings.Contains(response.Body.String(), "private-") || strings.Contains(logs.String(), "private-") {
				t.Fatal("原结果被修改或交付了内部原文", err)
			}
		})
	}
}

func TestNameEntryPageHandlerAuthorization(t *testing.T) {
	for _, mode := range []string{"missing bearer", "cookie only", "query only", "invalid bearer"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.GET("/test-name-entry-page", middleware.RequireAuthorization(func(context.Context, string) (*domain.Actor, error) { return nil, domain.ErrInvalidAccessToken }, handler.AuthorizationFailure), handler.NameEntryPage(func(context.Context, domain.Actor, domain.GlobalEntryQuery, string) (*domain.NameEntryPageResult, error) {
				calls++
				return nil, nil
			}))
			request := httptest.NewRequest("GET", "/test-name-entry-page?limit=0", nil)
			switch mode {
			case "cookie only":
				request.AddCookie(&http.Cookie{Name: handler.RefreshCookieName, Value: "private-refresh"})
			case "query only":
				request.URL.RawQuery += "&access_token=private-access"
			case "invalid bearer":
				request.Header.Set("Authorization", "Bearer private-access")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			var body dto.ErrorResponse
			if json.Unmarshal(response.Body.Bytes(), &body) != nil || response.Code != 401 || body.Error.Code != "AUTH_REQUIRED" || body.Error.Details != nil || calls != 0 || body.RequestID == "" || body.RequestID != response.Header().Get("X-Request-ID") {
				t.Fatal("认证未先于名称查询参数校验", calls)
			}
		})
	}
}

func TestNameEntryPageHandlerCommittedResponse(t *testing.T) {
	for _, mode := range []string{"body", "headers"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.GET("/test-name-entry-page", func(requestContext *gin.Context) {
				if mode == "body" {
					requestContext.String(202, "already-written")
				} else {
					requestContext.Status(202)
					requestContext.Writer.WriteHeaderNow()
				}
			}, handler.NameEntryPage(func(context.Context, domain.Actor, domain.GlobalEntryQuery, string) (*domain.NameEntryPageResult, error) {
				calls++
				return nil, nil
			}), func(*gin.Context) { t.Error("已提交响应进入下游") })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest("GET", "/test-name-entry-page", nil))
			expected := ""
			if mode == "body" {
				expected = "already-written"
			}
			if response.Code != 202 || response.Body.String() != expected || calls != 0 {
				t.Fatal("已提交响应被修改或调用了服务")
			}
		})
	}
}

func TestNameEntryPageHandlerDependency(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("缺少名称分页服务仍构造成功")
		}
	}()
	handler.NameEntryPage(nil)
}
