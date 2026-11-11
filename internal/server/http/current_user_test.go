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
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestCurrentUserResponses(t *testing.T) {
	base := domain.UserProfile{ID: 7, Username: "fixture", DisplayName: "学习用户", Status: "ACTIVE", RootEntryID: 13, QuotaBytes: 100, UsedBytes: 60, ReservedBytes: 40}
	large, zero, wrongOwner, missingRoot, unsafe := base, base, base, base, base
	large.ID, large.RootEntryID = ^uint64(0), ^uint64(0)
	zero.QuotaBytes, zero.UsedBytes, zero.ReservedBytes = 0, 0, 0
	wrongOwner.ID, missingRoot.RootEntryID, unsafe.QuotaBytes = 99, 0, 1<<53
	for _, testCase := range []struct {
		name, code                  string
		profile                     *domain.UserProfile
		err                         error
		status                      int
		missingActor, before, after bool
		panicValue                  bool
	}{
		{name: "active", profile: &base, status: 200},
		{name: "maximum ids", profile: &large, status: 200},
		{name: "zero quota", profile: &zero, status: 200},
		{name: "not found", err: domain.ErrAccountNotFound, status: 401, code: "AUTH_REQUIRED"},
		{name: "wrapped not found", err: fmt.Errorf("private-wrapped: %w", domain.ErrAccountNotFound), status: 401, code: "AUTH_REQUIRED"},
		{name: "disabled", err: domain.ErrAccountDisabled, status: 403, code: "ACCOUNT_DISABLED"},
		{name: "data error", err: domain.ErrInvalidAccountData, status: 500, code: "INTERNAL_ERROR"},
		{name: "query fault", err: errors.New("private-database-password"), status: 500, code: "INTERNAL_ERROR"},
		{name: "deadline", err: context.DeadlineExceeded, status: 500, code: "INTERNAL_ERROR"},
		{name: "partial result with error", profile: &base, err: domain.ErrAccountDisabled, status: 403, code: "ACCOUNT_DISABLED"},
		{name: "nil profile", status: 500, code: "INTERNAL_ERROR"},
		{name: "wrong owner", profile: &wrongOwner, status: 500, code: "INTERNAL_ERROR"},
		{name: "missing root", profile: &missingRoot, status: 500, code: "INTERNAL_ERROR"},
		{name: "unsafe bytes", profile: &unsafe, status: 500, code: "INTERNAL_ERROR"},
		{name: "missing actor", missingActor: true, status: 500, code: "INTERNAL_ERROR"},
		{name: "already canceled", before: true, status: 500, code: "INTERNAL_ERROR"},
		{name: "canceled after query", after: true, profile: &base, status: 500, code: "INTERNAL_ERROR"},
		{name: "service panic", panicValue: true, status: 500, code: "INTERNAL_ERROR"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var logs bytes.Buffer
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if testCase.before {
				cancel()
			}
			userID := uint64(7)
			if testCase.name == "maximum ids" {
				userID = ^uint64(0)
			}
			calls, downstream := 0, 0
			getProfile := func(received context.Context, receivedID uint64) (*domain.UserProfile, error) {
				calls++
				if receivedID != userID || received.Done() != ctx.Done() || requestcontext.RequestID(received) == "" {
					t.Error("资料服务未收到 Actor 用户 ID 或原上下文信息")
				}
				if testCase.panicValue {
					panic("private-service-panic")
				}
				if testCase.after {
					cancel()
				}
				return testCase.profile, testCase.err
			}
			router := NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
			router.GET("/test-current-user", func(requestContext *gin.Context) {
				if !testCase.missingActor {
					actor := domain.Actor{UserID: userID, SessionID: 11, DeviceID: 13}
					actorContext := requestcontext.WithActor(requestContext.Request.Context(), actor)
					requestContext.Request = requestContext.Request.WithContext(actorContext)
				}
				requestContext.Next()
			}, handler.CurrentUser(getProfile), func(*gin.Context) { downstream++ })
			request := httptest.NewRequest("GET", "/test-current-user?user_id=999&token=private-query", strings.NewReader(`{"user_id":"999"}`)).WithContext(ctx)
			request.Header.Set("X-User-ID", "999")
			request.Header.Set("Origin", "http://127.0.0.1:5173")
			request.Header.Set("Authorization", "Bearer private-token")
			request.Header.Set("X-Request-ID", "28EE758A-0727-42DE-AE7D-3BB33155E148")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			expectedCalls := 1
			if testCase.before || testCase.missingActor {
				expectedCalls = 0
			}
			if response.Code != testCase.status || calls != expectedCalls || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Content-Type") != "application/json; charset=utf-8" {
				t.Fatal("处理函数状态、服务调用次数或响应头错误")
			}
			if response.Header().Get("Access-Control-Allow-Origin") != "http://127.0.0.1:5173" {
				t.Fatal("当前用户响应丢失 CORS 放行头")
			}
			var body map[string]any
			decoder := json.NewDecoder(bytes.NewReader(response.Body.Bytes()))
			decoder.UseNumber()
			if err := decoder.Decode(&body); err != nil || len(body) != 2 || body["request_id"] != response.Header().Get("X-Request-ID") {
				t.Fatal("响应结构或请求 ID 不一致")
			}
			if testCase.status == 200 {
				data, valid := body["data"].(map[string]any)
				if !valid || len(data) != 5 {
					t.Fatal("成功响应缺少资料字段")
				}
				user, valid := data["user"].(map[string]any)
				if !valid || user["id"] != strconv.FormatUint(userID, 10) || data["root_entry_id"] != strconv.FormatUint(testCase.profile.RootEntryID, 10) {
					t.Fatal("返回资料的用户或根目录 ID 错误")
				}
			} else {
				detail, valid := body["error"].(map[string]any)
				if !valid || len(detail) != 3 || detail["code"] != testCase.code || detail["details"] != nil || downstream != 0 {
					t.Fatal("错误正文或中止行为不符合契约")
				}
			}
			if testCase.status == 401 && response.Header().Get("WWW-Authenticate") != `Bearer realm="cloudnest"` {
				t.Fatal("认证失败缺少 Bearer 提示")
			}
			if strings.Contains(response.Body.String(), "private-") || strings.Contains(logs.String(), "private-") {
				t.Fatal("响应或日志泄漏敏感输入")
			}
			var record map[string]any
			if err := json.Unmarshal(logs.Bytes(), &record); err != nil || record["status"] != float64(testCase.status) || record["request_id"] != body["request_id"] {
				t.Fatal("完成日志与响应不一致")
			}
		})
	}
}

func TestCurrentUserDependency(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("缺少资料服务应暴露装配错误")
		}
	}()
	handler.CurrentUser(nil)
}

func TestCurrentUserWithJWT(t *testing.T) {
	// 1. 使用真实签发、校验及授权服务，数据库查询由内存状态提供方替代。
	cfg := config.JWTConfig{SigningKey: bytes.Repeat([]byte{7}, 32), Issuer: "cloudnest-test", Audience: "cloudnest-api-test", AccessTokenTTL: 15 * time.Minute, RefreshTokenTTL: time.Hour}
	signer, err := service.NewAccessTokenSigner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := service.NewAccessTokenVerifier(cfg)
	if err != nil {
		t.Fatal(err)
	}
	session := domain.LoginSession{ID: 11, UserID: 7, DeviceID: 13, ExpiresAt: time.Now().UTC().Add(time.Hour)}
	token, err := signer.Issue(session)
	if err != nil {
		t.Fatal("测试令牌签发失败")
	}
	authorization := service.NewAuthorizationService(verifier.Verify, func(context.Context, uint64, uint64) (*domain.SessionAuthorization, error) {
		return &domain.SessionAuthorization{UserID: 7, SessionID: 11, DeviceID: 13, UserStatus: "ACTIVE", SessionExpiresAt: session.ExpiresAt}, nil
	})
	profiles := service.NewUserProfileService(func(context.Context, uint64) (*domain.UserProfile, error) {
		return &domain.UserProfile{ID: 7, Username: "fixture", DisplayName: "学习用户", Status: "ACTIVE", RootEntryID: 17, QuotaBytes: 100}, nil
	})
	router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
	protected := middleware.RequireAuthorization(authorization.Authorize, handler.AuthorizationFailure)
	router.GET("/test-current-user", protected, handler.CurrentUser(profiles.Get))
	router.HEAD("/test-current-user", protected, handler.CurrentUser(profiles.Get))
	server := httptest.NewServer(router)
	defer server.Close()
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			request, err := http.NewRequest(method, server.URL+"/test-current-user?user_id=999", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+token.Token)
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal("真实 TCP 请求失败")
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != 200 || response.Header.Get("Cache-Control") != "no-store" {
				t.Fatal("网络响应状态或缓存设置异常")
			}
			if method == http.MethodHead {
				if len(body) != 0 {
					t.Fatal("HEAD 网络响应应为空正文")
				}
				return
			}
			var payload struct {
				Data struct {
					User struct {
						ID string `json:"id"`
					} `json:"user"`
					RootEntryID string `json:"root_entry_id"`
				} `json:"data"`
				RequestID string `json:"request_id"`
			}
			if err := json.Unmarshal(body, &payload); err != nil || payload.Data.User.ID != "7" || payload.Data.RootEntryID != "17" || payload.RequestID != response.Header.Get("X-Request-ID") {
				t.Fatal("真实 JWT 的用户资料响应不一致")
			}
		})
	}
}
