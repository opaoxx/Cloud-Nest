package http

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/security"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestBearerTokenHTTPAuthorization(t *testing.T) {
	// 1. 使用测试密钥构造真实签发器、校验器及内存会话查询。
	jwtConfig := config.JWTConfig{
		SigningKey: bytes.Repeat([]byte{7}, 32), Issuer: "cloudnest-test", Audience: "cloudnest-api-test",
		AccessTokenTTL: 15 * time.Minute, RefreshTokenTTL: 24 * time.Hour,
	}
	signer, err := service.NewAccessTokenSigner(jwtConfig)
	if err != nil {
		t.Fatal("测试签发器构造失败")
	}
	verifier, err := service.NewAccessTokenVerifier(jwtConfig)
	if err != nil {
		t.Fatal("测试校验器构造失败")
	}
	session := domain.LoginSession{ID: 11, UserID: 7, DeviceID: 13, ExpiresAt: time.Now().UTC().Add(time.Hour)}
	signed, err := signer.Issue(session)
	if err != nil {
		t.Fatal("测试令牌签发失败")
	}
	var queryCalls, authorizeCalls atomic.Int64
	authorizationService := service.NewAuthorizationService(verifier.Verify,
		func(ctx context.Context, userID, sessionID uint64) (*domain.SessionAuthorization, error) {
			queryCalls.Add(1)
			if ctx.Err() != nil || userID != session.UserID || sessionID != session.ID {
				t.Error("授权查询未收到有效上下文与已验证标识")
			}
			return &domain.SessionAuthorization{
				UserID: session.UserID, SessionID: session.ID, DeviceID: session.DeviceID,
				UserStatus: "ACTIVE", SessionExpiresAt: session.ExpiresAt,
			}, nil
		})

	// 2. 在生产公共中间件内增加测试专用入口，验证提取结果能够交给授权服务。
	var logs bytes.Buffer
	router := NewRouter(logging.New(&logs), nil, func(context.Context) bool { return true })
	router.GET("/test-bearer", func(requestContext *gin.Context) {
		tokenText, parseError := security.BearerToken(requestContext.Request)
		if parseError != nil {
			requestContext.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		authorizeCalls.Add(1)
		actor, authorizationError := authorizationService.Authorize(requestContext.Request.Context(), tokenText)
		if authorizationError != nil {
			requestContext.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		if actor.UserID != session.UserID || actor.SessionID != session.ID || actor.DeviceID != session.DeviceID {
			t.Error("HTTP 授权结果标识不一致")
		}
		requestContext.Status(http.StatusNoContent)
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	// 3. 通过真实 TCP 发送单头、重复头与异常凭证，观察授权和查询调用次数。
	for _, testCase := range []struct {
		name                    string
		values                  []string
		status                  int
		authorizations, queries int64
	}{
		{name: "valid", values: []string{"Bearer " + signed.Token}, status: 204, authorizations: 1, queries: 1},
		{name: "case and spaces", values: []string{"\t bEaReR   " + signed.Token + " \t"}, status: 204, authorizations: 1, queries: 1},
		{name: "missing with query and cookie", status: 401},
		{name: "duplicate identical", values: []string{"Bearer " + signed.Token, "Bearer " + signed.Token}, status: 401},
		{name: "duplicate empty", values: []string{"Bearer " + signed.Token, ""}, status: 401},
		{name: "comma joined", values: []string{"Bearer " + signed.Token + ", Bearer private-token"}, status: 401},
		{name: "tab separator", values: []string{"Bearer\t" + signed.Token}, status: 401},
		{name: "invalid jwt", values: []string{"Bearer private-token"}, status: 401, authorizations: 1},
		{name: "bearer syntax with padding", values: []string{"Bearer AZaz09-._~+/=="}, status: 401, authorizations: 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			authorizeCalls.Store(0)
			queryCalls.Store(0)
			request, err := http.NewRequest(http.MethodGet, server.URL+"/test-bearer?access_token="+signed.Token, nil)
			if err != nil {
				t.Fatal("测试请求构造失败")
			}
			request.Header.Set("Cookie", "access_token="+signed.Token)
			for _, value := range testCase.values {
				request.Header.Add("authorization", value)
			}
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal("测试 HTTP 请求失败")
			}
			body, readError := io.ReadAll(response.Body)
			closeError := response.Body.Close()
			if readError != nil || closeError != nil || len(body) != 0 || response.StatusCode != testCase.status {
				t.Fatal("测试入口响应异常")
			}
			if authorizeCalls.Load() != testCase.authorizations || queryCalls.Load() != testCase.queries {
				t.Fatal("异常凭证未在对应阶段结束处理")
			}
			if response.Header.Get("X-Request-ID") == "" {
				t.Fatal("请求未经过公共中间件")
			}
		})
	}
	if strings.Contains(logs.String(), signed.Token) || strings.Contains(logs.String(), "private-token") {
		t.Fatal("完成日志泄漏凭证")
	}
}
