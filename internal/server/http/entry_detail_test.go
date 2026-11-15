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

func TestEntryDetailHandlerFlow(t *testing.T) {
	for _, testCase := range []struct {
		name, path, code                                  string
		status, calls, cancelOn                           int
		entryID                                           uint64
		serviceError                                      error
		change                                            func(*domain.EntryDetail)
		missingActor, nilResult, maximumActor             bool
		cancelBefore, cancelAfter, deadline, panicService bool
		pending, noOrigin, forbiddenOrigin                bool
	}{
		{name: "file", status: 200, calls: 1},
		{name: "decoded path", path: "%32%33", status: 200, calls: 1},
		{name: "no origin", noOrigin: true, status: 200, calls: 1},
		{name: "pending success", pending: true, status: 200, calls: 1},
		{name: "root", path: "11", entryID: 11, status: 200, calls: 1, change: func(result *domain.EntryDetail) {
			result.Path.Entries = result.Path.Entries[:1]
			result.Display = domain.FileEntryDisplay{ID: 11, UserID: 7}
			result.Preference = domain.EntryPreference{UserID: 7, EntryID: 11}
		}},
		{name: "historical source", status: 200, calls: 1, change: func(result *domain.EntryDetail) { result.Display.SourceDeviceID = nil }},
		{name: "default preference", status: 200, calls: 1, change: func(result *domain.EntryDetail) {
			result.Preference.StarredAt, result.Preference.LastOpenedAt = nil, nil
		}},
		{name: "maximum identifiers", path: "18446744073709551615", entryID: ^uint64(0), maximumActor: true, status: 200, calls: 1},
		{name: "missing actor", missingActor: true, status: 500, code: "INTERNAL_ERROR"},
		{name: "zero id", path: "0", status: 400, code: "INVALID_ARGUMENT"},
		{name: "leading zero", path: "023", status: 400, code: "INVALID_ARGUMENT"},
		{name: "overflow", path: "18446744073709551616", status: 400, code: "INVALID_ARGUMENT"},
		{name: "double encoded", path: "%2532%2533", status: 400, code: "INVALID_ARGUMENT"},
		{name: "forbidden origin", forbiddenOrigin: true, status: 403, code: "CORS_FORBIDDEN"},
		{name: "missing target", serviceError: domain.ErrFileEntryNotFound, status: 404, calls: 1, code: "ENTRY_NOT_FOUND"},
		{name: "wrapped missing target", serviceError: fmt.Errorf("private-entry-error: %w", domain.ErrFileEntryNotFound), status: 404, calls: 1, code: "ENTRY_NOT_FOUND"},
		{name: "disabled account", serviceError: domain.ErrAccountDisabled, status: 403, calls: 1, code: "ACCOUNT_DISABLED"},
		{name: "revoked session", serviceError: domain.ErrSessionRevoked, status: 401, calls: 1, code: "SESSION_REVOKED"},
		{name: "missing session", serviceError: domain.ErrSessionNotFound, status: 401, calls: 1, code: "SESSION_REVOKED"},
		{name: "expired session", serviceError: domain.ErrSessionExpired, status: 401, calls: 1, code: "SESSION_REVOKED"},
		{name: "revoked device", serviceError: domain.ErrDeviceRevoked, status: 401, calls: 1, code: "SESSION_REVOKED"},
		{name: "missing account", serviceError: domain.ErrAccountNotFound, status: 401, calls: 1, code: "AUTH_REQUIRED"},
		{name: "data failure", serviceError: domain.ErrInvalidFileEntryData, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "query failure", serviceError: errors.New("private-query-failure"), status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "nil result", nilResult: true, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "empty path", status: 500, calls: 1, code: "INTERNAL_ERROR", change: func(result *domain.EntryDetail) { result.Path.Entries = nil }},
		{name: "wrong target result", status: 500, calls: 1, code: "INTERNAL_ERROR", change: func(result *domain.EntryDetail) {
			result.Path.Entries[2].ID, result.Display.ID, result.Preference.EntryID = 24, 24, 24
		}},
		{name: "foreign result", status: 500, calls: 1, code: "INTERNAL_ERROR", change: func(result *domain.EntryDetail) {
			for index := range result.Path.Entries {
				result.Path.Entries[index].UserID = 8
			}
			result.Display.UserID, result.Preference.UserID = 8, 8
		}},
		{name: "foreign display", status: 500, calls: 1, code: "INTERNAL_ERROR", change: func(result *domain.EntryDetail) { result.Display.UserID = 8 }},
		{name: "foreign preference", status: 500, calls: 1, code: "INTERNAL_ERROR", change: func(result *domain.EntryDetail) { result.Preference.UserID = 8 }},
		{name: "unsafe bytes", status: 500, calls: 1, code: "INTERNAL_ERROR", change: func(result *domain.EntryDetail) { result.Path.Entries[2].FileSize = 1 << 53 }},
		{name: "invalid time", status: 500, calls: 1, code: "INTERNAL_ERROR", change: func(result *domain.EntryDetail) { result.Path.Entries[2].CreatedAt = time.Time{} }},
		{name: "pending path error", path: "0", pending: true, status: 400, code: "INVALID_ARGUMENT"},
		{name: "pending not found", pending: true, serviceError: domain.ErrFileEntryNotFound, status: 404, calls: 1, code: "ENTRY_NOT_FOUND"},
		{name: "pending auth error", pending: true, serviceError: domain.ErrSessionRevoked, status: 401, calls: 1, code: "SESSION_REVOKED"},
		{name: "cancelled before entry", cancelBefore: true, status: 500, code: "INTERNAL_ERROR"},
		{name: "cancelled before service", cancelOn: 2, status: 500, code: "INTERNAL_ERROR"},
		{name: "cancelled after service", cancelAfter: true, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "cancelled with target error", cancelAfter: true, serviceError: domain.ErrFileEntryNotFound, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "cancelled at delivery", cancelOn: 4, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "cancelled before write", cancelOn: 5, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "service deadline", deadline: true, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "service panic", panicService: true, status: 500, calls: 1, code: "INTERNAL_ERROR"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 通过认证上下文传入身份，核对服务收到的目标及原上下文。
			actor := domain.Actor{UserID: 7, SessionID: 17, DeviceID: 19}
			result := entryDetailSuccessFixture()
			if testCase.maximumActor {
				maximum := ^uint64(0)
				actor = domain.Actor{UserID: maximum, SessionID: maximum, DeviceID: maximum}
				for index := range result.Path.Entries {
					entry := &result.Path.Entries[index]
					entry.ID, entry.UserID = maximum-2+uint64(index), maximum
					if index > 0 {
						parent := result.Path.Entries[index-1].ID
						entry.ParentID = &parent
					}
				}
				result.Display.ID, result.Display.UserID, result.Display.SourceDeviceID = maximum, maximum, &maximum
				result.Preference.UserID, result.Preference.EntryID = maximum, maximum
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
			targetID := testCase.entryID
			if targetID == 0 {
				targetID = 23
			}
			var expectedContext context.Context
			var logs bytes.Buffer
			calls, downstream := 0, 0
			router := NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
			detail := handler.EntryDetail(func(received context.Context, identity domain.Actor, target uint64) (*domain.EntryDetail, error) {
				calls++
				if received != expectedContext || identity != actor || target != targetID {
					t.Error("详情服务的上下文、身份或路径目标错误")
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
			router.GET("/test-entry-detail/:id", func(requestContext *gin.Context) {
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
			}, detail, func(*gin.Context) { downstream++ })
			path := testCase.path
			if path == "" {
				path = "23"
			}
			request := httptest.NewRequest("GET", "/test-entry-detail/"+path+"?id=99&user_id=99&note=private-query", strings.NewReader(`{"id":"99","user_id":"99"}`)).WithContext(ctx)
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

			// 2. 核对成功交付、错误分类、请求关联与头部。
			if response.Code != testCase.status || calls != testCase.calls || downstream != 0 || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Content-Type") != "application/json; charset=utf-8" || response.Header().Get("Content-Encoding") != "" || len(response.Header().Values("Set-Cookie")) != 0 {
				t.Fatal("详情状态、调用顺序或响应头错误", response.Code, calls, downstream)
			}
			if response.Header().Get("X-Request-ID") != "28ee758a-0727-42de-ae7d-3bb33155e148" || (testCase.code != "CORS_FORBIDDEN" && response.Header().Get("Pragma") != "no-cache") {
				t.Fatal("详情请求 ID 或缓存头错误")
			}
			if !testCase.noOrigin && !testCase.forbiddenOrigin && response.Header().Get("Access-Control-Allow-Origin") != "http://127.0.0.1:5173" {
				t.Fatal("详情 CORS 放行头错误")
			}
			if testCase.status == 200 {
				var body dto.EntryDetailResponse
				expected, err := dto.NewEntryDetailData(result)
				if err != nil || json.Unmarshal(response.Body.Bytes(), &body) != nil || !reflect.DeepEqual(body.Data, *expected) || body.Data.ID != strconv.FormatUint(targetID, 10) || body.RequestID != response.Header().Get("X-Request-ID") || response.Header().Get("Content-Length") != strconv.Itoa(response.Body.Len()) {
					t.Fatal("完整详情成功交付错误", err)
				}
			} else {
				var body dto.ErrorResponse
				if json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Error.Code != testCase.code || body.RequestID != response.Header().Get("X-Request-ID") || response.Header().Get("Content-Length") != "" {
					t.Fatal("详情统一错误结构或业务码错误")
				}
				if testCase.code == "INVALID_ARGUMENT" {
					if len(body.Error.Details) != 1 || body.Error.Details["field"] != "id" || body.Error.Message != "目录项 ID 须为 1～18446744073709551615 的标准十进制整数" {
						t.Fatal("路径字段错误内容不准确")
					}
				} else if body.Error.Details != nil {
					t.Fatal("非字段错误 details 非空")
				}
				if testCase.code == "ENTRY_NOT_FOUND" && body.Error.Message != "目录项不存在" {
					t.Fatal("不可见目标公开提示错误")
				}
			}
			if (testCase.status == 401) != (response.Header().Get("WWW-Authenticate") != "") {
				t.Fatal("详情认证提示与状态不一致")
			}

			// 3. 公开正文与日志保护请求值及内部故障，日志保护详情正文。
			if strings.Contains(response.Body.String(), "private-") || strings.Contains(logs.String(), "private-") {
				t.Fatal("详情正文或日志包含请求及内部故障原文")
			}
			if strings.Contains(logs.String(), "学习笔记.md") || strings.Contains(logs.String(), "发布时设备 🎮") {
				t.Fatal("详情完成日志包含展示正文")
			}
		})
	}
}

func TestEntryDetailHandlerAuthorization(t *testing.T) {
	for _, mode := range []string{"missing bearer", "cookie only", "query only", "invalid bearer"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.GET("/test-entry-detail/:id", middleware.RequireAuthorization(func(context.Context, string) (*domain.Actor, error) { return nil, domain.ErrInvalidAccessToken }, handler.AuthorizationFailure), handler.EntryDetail(func(context.Context, domain.Actor, uint64) (*domain.EntryDetail, error) {
				calls++
				return nil, nil
			}))
			request := httptest.NewRequest("GET", "/test-entry-detail/0", nil)
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
				t.Fatal("认证未先于详情路径校验", calls)
			}
		})
	}
}

func TestEntryDetailHandlerCommittedResponse(t *testing.T) {
	for _, mode := range []string{"body", "headers"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.GET("/test-entry-detail/:id", func(requestContext *gin.Context) {
				if mode == "body" {
					requestContext.String(202, "already-written")
				} else {
					requestContext.Status(202)
					requestContext.Writer.WriteHeaderNow()
				}
			}, handler.EntryDetail(func(context.Context, domain.Actor, uint64) (*domain.EntryDetail, error) {
				calls++
				return nil, nil
			}), func(*gin.Context) { t.Error("已提交响应进入下游") })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest("GET", "/test-entry-detail/23", nil))
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

func TestEntryDetailHandlerDependency(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("缺少详情服务仍构造成功")
		}
	}()
	handler.EntryDetail(nil)
}
