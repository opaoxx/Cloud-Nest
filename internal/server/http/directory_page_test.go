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

func TestDirectoryPageHandlerFlow(t *testing.T) {
	for _, testCase := range []struct {
		name, raw, code, field, cursor                    string
		status, calls, size, cancelOn                     int
		parentID                                          uint64
		serviceError                                      error
		change                                            func(*domain.DirectoryPageResult)
		missingActor, nilResult, maximumActor             bool
		cancelBefore, cancelAfter, deadline, panicService bool
		pending, noOrigin, forbiddenOrigin                bool
	}{
		{name: "defaults", status: 200, calls: 1},
		{name: "next page", raw: "parent_id=12&cursor=private-opaque-cursor&limit=2", cursor: "private-opaque-cursor", size: 2, status: 200, calls: 1},
		{name: "explicit first cursor", raw: "parent_id=12&cursor=", status: 200, calls: 1},
		{name: "encoded input", raw: "parent%5Fid=%31%32&limit=%31", size: 1, status: 200, calls: 1},
		{name: "supported mode", raw: "scope=directory&sort=modified_desc&q=&kind=&parent_id=12", status: 200, calls: 1},
		{name: "no origin", noOrigin: true, status: 200, calls: 1},
		{name: "pending success", pending: true, status: 200, calls: 1},
		{name: "empty page", status: 200, calls: 1, change: func(result *domain.DirectoryPageResult) { result.Entries, result.NextCursor = nil, nil }},
		{name: "last page", status: 200, calls: 1, change: func(result *domain.DirectoryPageResult) { result.NextCursor = nil }},
		{name: "maximum identifiers", raw: "parent_id=18446744073709551614&limit=100", parentID: ^uint64(0) - 1, size: 100, maximumActor: true, status: 200, calls: 1},
		{name: "missing actor", missingActor: true, status: 500, code: "INTERNAL_ERROR"},
		{name: "missing parent", raw: "&", status: 400, code: "INVALID_ARGUMENT", field: "parent_id"},
		{name: "zero parent", raw: "parent_id=0", status: 400, code: "INVALID_ARGUMENT", field: "parent_id"},
		{name: "duplicate parent", raw: "parent_id=12&parent%5Fid=12", status: 400, code: "INVALID_ARGUMENT", field: "parent_id"},
		{name: "bad limit", raw: "parent_id=12&limit=0", status: 400, code: "INVALID_ARGUMENT", field: "limit"},
		{name: "malformed query", raw: "parent_id=12&extra=%ZZ", status: 400, code: "INVALID_ARGUMENT", field: "query"},
		{name: "unsupported scope", raw: "parent_id=12&scope=all", status: 400, code: "INVALID_ARGUMENT", field: "scope"},
		{name: "forbidden origin", forbiddenOrigin: true, status: 403, code: "CORS_FORBIDDEN"},
		{name: "invalid cursor", serviceError: domain.ErrInvalidDirectoryCursor, status: 400, calls: 1, code: "INVALID_CURSOR", field: "cursor"},
		{name: "wrapped invalid cursor", serviceError: fmt.Errorf("private-cursor-error: %w", domain.ErrInvalidDirectoryCursor), status: 400, calls: 1, code: "INVALID_CURSOR", field: "cursor"},
		{name: "missing entry", serviceError: domain.ErrFileEntryNotFound, status: 404, calls: 1, code: "ENTRY_NOT_FOUND"},
		{name: "wrapped missing entry", serviceError: fmt.Errorf("private-entry-error: %w", domain.ErrFileEntryNotFound), status: 404, calls: 1, code: "ENTRY_NOT_FOUND"},
		{name: "file parent", serviceError: domain.ErrFileEntryNotDirectory, status: 400, calls: 1, code: "INVALID_ARGUMENT", field: "parent_id"},
		{name: "disabled account", serviceError: domain.ErrAccountDisabled, status: 403, calls: 1, code: "ACCOUNT_DISABLED"},
		{name: "revoked session", serviceError: domain.ErrSessionRevoked, status: 401, calls: 1, code: "SESSION_REVOKED"},
		{name: "missing session", serviceError: domain.ErrSessionNotFound, status: 401, calls: 1, code: "SESSION_REVOKED"},
		{name: "expired session", serviceError: domain.ErrSessionExpired, status: 401, calls: 1, code: "SESSION_REVOKED"},
		{name: "revoked device", serviceError: domain.ErrDeviceRevoked, status: 401, calls: 1, code: "SESSION_REVOKED"},
		{name: "missing account", serviceError: domain.ErrAccountNotFound, status: 401, calls: 1, code: "AUTH_REQUIRED"},
		{name: "data failure", serviceError: domain.ErrInvalidFileEntryData, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "database failure", serviceError: errors.New("private-database-failure"), status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "nil result", nilResult: true, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "empty path", status: 500, calls: 1, code: "INTERNAL_ERROR", change: func(result *domain.DirectoryPageResult) { result.Path.Entries = nil }},
		{name: "wrong result parent", status: 500, calls: 1, code: "INTERNAL_ERROR", change: func(result *domain.DirectoryPageResult) {
			parent := uint64(99)
			result.Path.Entries[1].ID = 99
			result.Entries[0].ParentID = &parent
		}},
		{name: "foreign result", status: 500, calls: 1, code: "INTERNAL_ERROR", change: func(result *domain.DirectoryPageResult) {
			for index := range result.Path.Entries {
				result.Path.Entries[index].UserID = 8
			}
			result.Entries[0].UserID = 8
		}},
		{name: "foreign later item", status: 500, calls: 1, code: "INTERNAL_ERROR", change: func(result *domain.DirectoryPageResult) {
			other := result.Entries[0]
			other.ID, other.UserID = 24, 8
			result.Entries = append(result.Entries, other)
		}},
		{name: "unsafe byte result", status: 500, calls: 1, code: "INTERNAL_ERROR", change: func(result *domain.DirectoryPageResult) { result.Entries[0].FileSize = 1 << 53 }},
		{name: "invalid result time", status: 500, calls: 1, code: "INTERNAL_ERROR", change: func(result *domain.DirectoryPageResult) { result.Entries[0].CreatedAt = time.Time{} }},
		{name: "pending query error", raw: "parent_id=0", pending: true, status: 400, code: "INVALID_ARGUMENT", field: "parent_id"},
		{name: "pending cursor error", pending: true, serviceError: domain.ErrInvalidDirectoryCursor, status: 400, calls: 1, code: "INVALID_CURSOR", field: "cursor"},
		{name: "pending not found", pending: true, serviceError: domain.ErrFileEntryNotFound, status: 404, calls: 1, code: "ENTRY_NOT_FOUND"},
		{name: "pending type error", pending: true, serviceError: domain.ErrFileEntryNotDirectory, status: 400, calls: 1, code: "INVALID_ARGUMENT", field: "parent_id"},
		{name: "pending auth error", pending: true, serviceError: domain.ErrSessionRevoked, status: 401, calls: 1, code: "SESSION_REVOKED"},
		{name: "cancelled before entry", cancelBefore: true, status: 500, code: "INTERNAL_ERROR"},
		{name: "cancelled before service", cancelOn: 2, status: 500, code: "INTERNAL_ERROR"},
		{name: "cancelled after service", cancelAfter: true, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "cancelled with cursor error", cancelAfter: true, serviceError: domain.ErrInvalidDirectoryCursor, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "cancelled at delivery", cancelOn: 4, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "cancelled before write", cancelOn: 5, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "service deadline", deadline: true, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "service panic", panicService: true, status: 500, calls: 1, code: "INTERNAL_ERROR"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 装配可信 Actor 和服务替身，核对本次完整上下文及输入。
			actor := domain.Actor{UserID: 7, SessionID: 17, DeviceID: 19}
			result := directoryPageSuccessFixture()
			if testCase.maximumActor {
				maximum := ^uint64(0)
				rootID, parentID := maximum-2, maximum-1
				actor = domain.Actor{UserID: maximum, SessionID: maximum, DeviceID: maximum}
				result.Path.Entries[0].ID, result.Path.Entries[0].UserID = rootID, maximum
				result.Path.Entries[1].ID, result.Path.Entries[1].UserID, result.Path.Entries[1].ParentID = parentID, maximum, &rootID
				result.Entries[0].ID, result.Entries[0].UserID, result.Entries[0].ParentID = maximum, maximum, &parentID
			}
			if testCase.change != nil {
				testCase.change(result)
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
			parentID, size := testCase.parentID, testCase.size
			if parentID == 0 {
				parentID = 12
			}
			if size == 0 {
				size = 50
			}
			var expectedContext context.Context
			var logs bytes.Buffer
			calls, downstream := 0, 0
			router := NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
			list := handler.DirectoryPage(func(received context.Context, identity domain.Actor, parent uint64, cursor string, limit int) (*domain.DirectoryPageResult, error) {
				calls++
				if received != expectedContext || identity != actor || parent != parentID || cursor != testCase.cursor || limit != size {
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
			router.GET("/test-directory-page", func(requestContext *gin.Context) {
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
			raw := testCase.raw
			if raw == "" {
				raw = "parent_id=12"
			}
			request := httptest.NewRequest("GET", "/test-directory-page", strings.NewReader(`{"user_id":"99","parent_id":"99"}`)).WithContext(ctx)
			request.URL.RawQuery = raw + "&user_id=99&note=private-query"
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

			// 2. 核对成功、固定业务错误、字段定位及响应头。
			if response.Code != testCase.status || calls != testCase.calls || downstream != 0 || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Content-Type") != "application/json; charset=utf-8" || response.Header().Get("Content-Encoding") != "" || len(response.Header().Values("Set-Cookie")) != 0 {
				t.Fatal("状态、调用顺序或响应头错误", response.Code, calls, downstream)
			}
			if response.Header().Get("X-Request-ID") != "28ee758a-0727-42de-ae7d-3bb33155e148" || (testCase.code != "CORS_FORBIDDEN" && response.Header().Get("Pragma") != "no-cache") {
				t.Fatal("请求 ID 或禁止缓存头错误")
			}
			if testCase.status == 200 {
				var body dto.DirectoryPageResponse
				if json.Unmarshal(response.Body.Bytes(), &body) != nil || body.RequestID != response.Header().Get("X-Request-ID") || len(body.Data.Items) != len(result.Entries) || body.Data.Breadcrumbs[len(body.Data.Breadcrumbs)-1].ID != strconv.FormatUint(parentID, 10) || response.Header().Get("Content-Length") != strconv.Itoa(response.Body.Len()) {
					t.Fatal("目录成功交付错误")
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
				if testCase.code == "INVALID_CURSOR" && body.Error.Message != "目录分页游标无效，请重新查询" {
					t.Fatal("无效游标公开提示错误")
				}
				if testCase.code == "ENTRY_NOT_FOUND" && body.Error.Message != "目录项不存在" {
					t.Fatal("不可见目录公开提示错误")
				}
				if testCase.serviceError == domain.ErrFileEntryNotDirectory && body.Error.Message != "父目录须为文件夹" {
					t.Fatal("文件父项公开提示错误")
				}
			}
			if (testCase.status == 401) != (response.Header().Get("WWW-Authenticate") != "") {
				t.Fatal("认证提示与状态不一致")
			}

			// 3. 原始查询、凭证及内部故障保持在公开正文与日志之外。
			if strings.Contains(response.Body.String(), "private-") || strings.Contains(logs.String(), "private-") {
				t.Fatal("响应或日志包含请求及内部故障原文")
			}
		})
	}
}

func TestDirectoryPageHandlerAuthorization(t *testing.T) {
	for _, mode := range []string{"missing bearer", "cookie only", "query only", "invalid bearer"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.GET("/test-directory-page", middleware.RequireAuthorization(func(context.Context, string) (*domain.Actor, error) { return nil, domain.ErrInvalidAccessToken }, handler.AuthorizationFailure), handler.DirectoryPage(func(context.Context, domain.Actor, uint64, string, int) (*domain.DirectoryPageResult, error) {
				calls++
				return nil, nil
			}))
			request := httptest.NewRequest("GET", "/test-directory-page?parent_id=0", nil)
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
				t.Fatal("认证未先于目录参数校验", calls)
			}
		})
	}
}

func TestDirectoryPageHandlerCommittedResponse(t *testing.T) {
	for _, mode := range []string{"body", "headers"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.GET("/test-directory-page", func(requestContext *gin.Context) {
				if mode == "body" {
					requestContext.String(202, "already-written")
				} else {
					requestContext.Status(202)
					requestContext.Writer.WriteHeaderNow()
				}
			}, handler.DirectoryPage(func(context.Context, domain.Actor, uint64, string, int) (*domain.DirectoryPageResult, error) {
				calls++
				return nil, nil
			}), func(*gin.Context) { t.Error("已提交响应进入下游") })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest("GET", "/test-directory-page", nil))
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

func TestDirectoryPageHandlerDependency(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("缺少分页服务仍构造成功")
		}
	}()
	handler.DirectoryPage(nil)
}
