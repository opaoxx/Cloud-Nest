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

func userSessionsHandlerFixture() *domain.UserSessionList {
	createdAt := time.Date(2026, 11, 13, 8, 0, 0, 123456789, time.FixedZone("UTC+8", 8*60*60))
	return &domain.UserSessionList{Sessions: []domain.UserSessionListItem{{
		UserSession: domain.UserSession{ID: 11, UserID: 7, DeviceID: 13, DeviceName: "学习设备", DevicePlatform: "WEB", CreatedAt: createdAt, ExpiresAt: createdAt.Add(time.Hour)},
		Current:     true,
	}}}
}

// cancelUserSessionsContext 在指定次数的 Err 检查时取消请求。
type cancelUserSessionsContext struct {
	context.Context
	cancel   context.CancelFunc
	cancelOn int
	checks   int
}

func (ctx *cancelUserSessionsContext) Err() error {
	ctx.checks++
	if ctx.checks == ctx.cancelOn {
		ctx.cancel()
	}
	return ctx.Context.Err()
}

func TestUserSessionsHandlerFlow(t *testing.T) {
	for _, testCase := range []struct {
		name, raw, code, field                             string
		status, calls, pageSize, cancelOn                  int
		beforeID                                           uint64
		change                                             func(*domain.UserSessionList)
		serviceError                                       error
		missingActor, nilResult, cancelBefore, cancelAfter bool
		deadline, panicService, maximumActor, noOrigin     bool
		forbiddenOrigin, pending                           bool
	}{
		{name: "defaults", status: 200, calls: 1, pageSize: 50},
		{name: "no origin", status: 200, calls: 1, pageSize: 50, noOrigin: true},
		{name: "explicit first page", raw: "before_id=0&limit=50", status: 200, calls: 1, pageSize: 50},
		{name: "next page", raw: "before_id=13&limit=2", status: 200, calls: 1, beforeID: 13, pageSize: 2},
		{name: "full unsigned boundary", raw: "before_id=18446744073709551615&limit=100", status: 200, calls: 1, beforeID: ^uint64(0), pageSize: 100},
		{name: "full unsigned actor", status: 200, calls: 1, pageSize: 50, maximumActor: true},
		{name: "empty page", status: 200, calls: 1, pageSize: 50, change: func(result *domain.UserSessionList) { result.Sessions = nil }},
		{name: "more pages", raw: "limit=1", status: 200, calls: 1, pageSize: 1, change: func(result *domain.UserSessionList) { result.HasMore = true }},
		{name: "pending body headers", status: 200, calls: 1, pageSize: 50, pending: true},
		{name: "missing actor", status: 500, code: "INTERNAL_ERROR", missingActor: true},
		{name: "invalid page size", raw: "limit=101", status: 400, code: "INVALID_ARGUMENT", field: "limit"},
		{name: "duplicate page size", raw: "limit=1&limit=2", status: 400, code: "INVALID_ARGUMENT", field: "limit"},
		{name: "invalid boundary", raw: "before_id=private-boundary", status: 400, code: "INVALID_ARGUMENT", field: "before_id"},
		{name: "malformed query", raw: "limit=%ZZ", status: 400, code: "INVALID_ARGUMENT", field: "query"},
		{name: "forbidden origin", status: 403, code: "CORS_FORBIDDEN", forbiddenOrigin: true},
		{name: "account disabled", status: 403, calls: 1, code: "ACCOUNT_DISABLED", serviceError: domain.ErrAccountDisabled},
		{name: "revoked session", status: 401, calls: 1, code: "SESSION_REVOKED", serviceError: domain.ErrSessionRevoked},
		{name: "wrapped session failure", status: 401, calls: 1, code: "SESSION_REVOKED", serviceError: fmt.Errorf("private-error: %w", domain.ErrSessionNotFound)},
		{name: "query failure", status: 500, calls: 1, code: "INTERNAL_ERROR", serviceError: errors.New("private-database-failure")},
		{name: "invalid service data", status: 500, calls: 1, code: "INTERNAL_ERROR", serviceError: domain.ErrInvalidSessionData},
		{name: "nil result", status: 500, calls: 1, code: "INTERNAL_ERROR", nilResult: true},
		{name: "wrong owner in later item", status: 500, calls: 1, code: "INTERNAL_ERROR", change: func(result *domain.UserSessionList) {
			foreign := result.Sessions[0]
			foreign.ID, foreign.UserID, foreign.DeviceName = 9, 99, "private-foreign-device"
			result.Sessions = append(result.Sessions, foreign)
		}},
		{name: "invalid timestamp", status: 500, calls: 1, code: "INTERNAL_ERROR", change: func(result *domain.UserSessionList) { result.Sessions[0].CreatedAt = time.Time{} }},
		{name: "canceled before reading", status: 500, code: "INTERNAL_ERROR", cancelBefore: true},
		{name: "canceled before service", status: 500, code: "INTERNAL_ERROR", cancelOn: 2},
		{name: "canceled after service", status: 500, calls: 1, code: "INTERNAL_ERROR", cancelAfter: true},
		{name: "canceled before response", status: 500, calls: 1, code: "INTERNAL_ERROR", cancelOn: 4},
		{name: "service deadline", status: 500, calls: 1, code: "INTERNAL_ERROR", deadline: true},
		{name: "service panic", status: 500, calls: 1, code: "INTERNAL_ERROR", panicService: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			actor := domain.Actor{UserID: 7, SessionID: 11, DeviceID: 13}
			result := userSessionsHandlerFixture()
			if testCase.maximumActor {
				actor = domain.Actor{UserID: ^uint64(0), SessionID: ^uint64(0), DeviceID: ^uint64(0)}
				result.Sessions[0].ID, result.Sessions[0].UserID, result.Sessions[0].DeviceID = actor.SessionID, actor.UserID, actor.DeviceID
			}
			if testCase.change != nil {
				testCase.change(result)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if testCase.deadline {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, 20*time.Millisecond)
				defer stop()
			}
			if testCase.cancelBefore {
				cancel()
			}
			var logs bytes.Buffer
			router := NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
			calls, downstream := 0, 0
			var expectedContext context.Context
			list := handler.UserSessions(func(received context.Context, identity domain.Actor, beforeID uint64, pageSize int) (*domain.UserSessionList, error) {
				calls++
				if received != expectedContext || identity != actor || beforeID != testCase.beforeID || (testCase.pageSize != 0 && pageSize != testCase.pageSize) {
					t.Error("列表服务未收到准确身份、上下文或分页参数")
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
			router.GET("/test-user-sessions", func(requestContext *gin.Context) {
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
			request := httptest.NewRequest("GET", "/test-user-sessions", strings.NewReader(`{"user_id":"99"}`)).WithContext(ctx)
			request.URL.RawQuery = "user_id=99&note=private-query"
			if testCase.raw != "" {
				request.URL.RawQuery += "&" + testCase.raw
			}
			request.RequestURI = request.URL.RequestURI()
			request.Header.Set("Authorization", "Bearer private-access")
			request.AddCookie(&http.Cookie{Name: "cloudnest_refresh_token", Value: "private-refresh"})
			request.Header.Set("X-User-ID", "99")
			request.Header.Set("X-Request-ID", "28EE758A-0727-42DE-AE7D-3BB33155E148")
			if !testCase.noOrigin {
				request.Header.Set("Origin", "http://127.0.0.1:5173")
			}
			if testCase.forbiddenOrigin {
				request.Header.Set("Origin", "https://other.example")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != testCase.status || calls != testCase.calls || downstream != 0 || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Set-Cookie") != "" {
				t.Fatal("状态、服务调用、结束处理或响应头错误", response.Code, calls, downstream)
			}
			if testCase.code != "CORS_FORBIDDEN" && response.Header().Get("Pragma") != "no-cache" {
				t.Error("缺少 Pragma 响应头")
			}
			if response.Header().Get("X-Request-ID") != "28ee758a-0727-42de-ae7d-3bb33155e148" {
				t.Fatal("请求 ID 错误")
			}
			if testCase.status == 200 {
				var body dto.UserSessionListResponse
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.RequestID != response.Header().Get("X-Request-ID") || body.Data.Items == nil ||
					len(body.Data.Items) != len(result.Sessions) || body.Data.HasMore != result.HasMore || response.Header().Get("Content-Type") != "application/json; charset=utf-8" ||
					response.Header().Get("Content-Encoding") != "" || response.Header().Get("Content-Length") != strconv.Itoa(response.Body.Len()) {
					t.Fatal("列表 JSON、请求 ID 或正文头错误", err)
				}
				for index, item := range body.Data.Items {
					if item.ID != strconv.FormatUint(result.Sessions[index].ID, 10) || item.Device.ID != strconv.FormatUint(result.Sessions[index].DeviceID, 10) || item.Current != result.Sessions[index].Current {
						t.Fatal("列表标识或当前标记错误")
					}
				}
			} else {
				var body dto.ErrorResponse
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Error.Code != testCase.code || body.RequestID != response.Header().Get("X-Request-ID") {
					t.Fatal("统一错误响应或请求 ID 错误", err)
				}
				if testCase.field == "" {
					if body.Error.Details != nil {
						t.Fatal("非字段错误的 details 应为空")
					}
				} else if len(body.Error.Details) != 1 || body.Error.Details["field"] != testCase.field {
					t.Fatal("查询字段位置错误")
				}
				if testCase.status == 401 && response.Header().Get("WWW-Authenticate") != `Bearer realm="cloudnest"` {
					t.Fatal("认证错误缺少 Bearer 提示")
				}
			}
			if strings.Contains(response.Body.String(), "private-") || strings.Contains(logs.String(), "private-") {
				t.Error("响应或日志包含查询、凭证或内部信息")
			}
		})
	}
}

func TestUserSessionsHandlerAuthorization(t *testing.T) {
	for _, candidate := range []string{"missing bearer", "cookie only", "query only", "invalid bearer"} {
		t.Run(candidate, func(t *testing.T) {
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			calls := 0
			router.GET("/test-user-sessions", middleware.RequireAuthorization(func(context.Context, string) (*domain.Actor, error) { return nil, domain.ErrInvalidAccessToken }, handler.AuthorizationFailure),
				handler.UserSessions(func(context.Context, domain.Actor, uint64, int) (*domain.UserSessionList, error) {
					calls++
					return nil, nil
				}))
			request := httptest.NewRequest("GET", "/test-user-sessions?limit=0", nil)
			switch candidate {
			case "cookie only":
				request.AddCookie(&http.Cookie{Name: "cloudnest_refresh_token", Value: "private-refresh"})
			case "query only":
				request.URL.RawQuery += "&access_token=private-access"
			case "invalid bearer":
				request.Header.Set("Authorization", "Bearer private-access")
			}
			request.RequestURI = request.URL.RequestURI()
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			var body dto.ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != 401 || body.Error.Code != "AUTH_REQUIRED" || body.Error.Details != nil || calls != 0 {
				t.Fatal("未认证请求进入了列表查询或参数校验", err)
			}
		})
	}
}

func TestUserSessionsHandlerCommittedResponse(t *testing.T) {
	router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
	router.GET("/test-user-sessions", func(requestContext *gin.Context) { requestContext.String(202, "already-written") },
		handler.UserSessions(func(context.Context, domain.Actor, uint64, int) (*domain.UserSessionList, error) {
			t.Error("响应已提交仍调用服务")
			return nil, nil
		}),
		func(*gin.Context) { t.Error("响应已提交后执行下游") })
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("GET", "/test-user-sessions", nil))
	if response.Code != 202 || response.Body.String() != "already-written" || response.Header().Get("Set-Cookie") != "" {
		t.Fatal("已提交响应被改写或追加")
	}
}

func TestUserSessionsHandlerDependency(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("缺少列表服务仍构造成功")
		}
	}()
	handler.UserSessions(nil)
}
