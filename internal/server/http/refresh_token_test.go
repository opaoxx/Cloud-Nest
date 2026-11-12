package http

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/security"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestRefreshTokenHTTPFlow(t *testing.T) {
	raw, expectedHash := service.GenerateRefreshToken()
	validCookie := security.RefreshCookieName + "=" + raw
	for _, testCase := range []struct {
		name, clientType, code                                                       string
		cookies                                                                      []string
		status, queries                                                              int
		missingBody, webBodyToken, badBody, missingOrigin, nativeOrigin, wrongHeader bool
	}{
		{name: "web", clientType: "web", cookies: []string{validCookie}, status: 204, queries: 1},
		{name: "native", clientType: "native", status: 204, queries: 1},
		{name: "native uses body with cookie", clientType: "native", cookies: []string{security.RefreshCookieName + "=private-cookie"}, status: 204, queries: 1},
		{name: "web missing cookie", clientType: "web", status: 401, code: "AUTH_REQUIRED"},
		{name: "web empty cookie", clientType: "web", cookies: []string{security.RefreshCookieName + "="}, status: 401, code: "AUTH_REQUIRED"},
		{name: "web duplicate cookie", clientType: "web", cookies: []string{validCookie, validCookie}, status: 401, code: "AUTH_REQUIRED"},
		{name: "web malformed duplicate", clientType: "web", cookies: []string{validCookie, security.RefreshCookieName + "=bad\\value"}, status: 401, code: "AUTH_REQUIRED"},
		{name: "web malformed format", clientType: "web", cookies: []string{security.RefreshCookieName + "=private-bad-token"}, status: 401, code: "AUTH_REQUIRED"},
		{name: "native malformed format", clientType: "native", badBody: true, status: 401, code: "AUTH_REQUIRED"},
		{name: "native empty body with cookie", clientType: "native", missingBody: true, cookies: []string{validCookie}, status: 400, code: "INVALID_ARGUMENT"},
		{name: "web body with cookie", clientType: "web", webBodyToken: true, cookies: []string{validCookie}, status: 400, code: "INVALID_ARGUMENT"},
		{name: "web missing origin", clientType: "web", missingOrigin: true, cookies: []string{validCookie}, status: 403, code: "CLIENT_SOURCE_FORBIDDEN"},
		{name: "native with origin", clientType: "native", nativeOrigin: true, status: 403, code: "CLIENT_SOURCE_FORBIDDEN"},
		{name: "client header mismatch", clientType: "web", wrongHeader: true, cookies: []string{validCookie}, status: 403, code: "CLIENT_SOURCE_FORBIDDEN"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var logs bytes.Buffer
			origin := "http://127.0.0.1:5173"
			router := NewRouter(logging.New(&logs), []string{origin}, func(context.Context) bool { return true })
			policy := security.NewLoginSourcePolicy([]string{origin})
			queries := 0
			credentials := service.NewRefreshCredentialService(func(context.Context, [sha256.Size]byte) (*domain.RefreshSession, error) {
				queries++
				return &domain.RefreshSession{SessionAuthorization: domain.SessionAuthorization{UserID: 7, SessionID: 11, DeviceID: 13, UserStatus: "ACTIVE", SessionExpiresAt: time.Now().UTC().Add(time.Hour)}, TokenGeneration: 1}, nil
			})
			accepted := false
			router.POST("/test-refresh-token", func(requestContext *gin.Context) {
				var input dto.RefreshRequest
				if !binding.ReadJSON(requestContext, &input) {
					return
				}
				if validationError := input.Validate(); validationError != nil {
					handler.InvalidArgument(requestContext, validationError)
					return
				}
				if !policy.Allows(requestContext.Request, input.ClientType) {
					handler.ClientSourceForbidden(requestContext)
					return
				}
				extracted, err := security.RefreshToken(requestContext.Request, input)
				if err != nil {
					handler.AuthorizationFailure(requestContext, err)
					return
				}
				verified, err := credentials.Verify(requestContext.Request.Context(), extracted)
				if err != nil {
					handler.AuthorizationFailure(requestContext, fmt.Errorf("fixture wrapped: %w", err))
					return
				}
				if verified.TokenHash != expectedHash || verified.Session.UserID != 7 {
					t.Error("完整提取链路取得错误原文或标识")
				}
				accepted = true
				requestContext.Status(204)
			})
			input := dto.RefreshRequest{ClientType: testCase.clientType}
			if testCase.clientType == "native" && !testCase.missingBody || testCase.webBodyToken {
				input.RefreshToken = raw
			}
			if testCase.badBody {
				input.RefreshToken = "private-bad-token"
			}
			encoded, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest("POST", "/test-refresh-token?refresh_token="+raw, bytes.NewReader(encoded))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set(security.ClientHeader, testCase.clientType)
			request.Header.Set("Authorization", "Bearer "+raw)
			if testCase.wrongHeader {
				request.Header.Set(security.ClientHeader, "native")
			}
			if testCase.clientType == "web" && !testCase.missingOrigin || testCase.nativeOrigin {
				request.Header.Set("Origin", origin)
			}
			for _, cookie := range testCase.cookies {
				request.Header.Add("Cookie", cookie)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != testCase.status || accepted != (testCase.status == 204) || queries != testCase.queries {
				t.Fatal("状态、后续调用或摘要查询次数不正确", response.Code, queries)
			}
			if response.Header().Get("Set-Cookie") != "" {
				t.Error("输入验证阶段写入了刷新 Cookie")
			}
			if testCase.status != 204 {
				var body dto.ErrorResponse
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Error.Code != testCase.code || body.RequestID == "" || body.RequestID != response.Header().Get("X-Request-ID") || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("失败响应结构错误", err)
				}
				if testCase.status == 401 && (body.Error.Details != nil || response.Header().Get("WWW-Authenticate") != `Bearer realm="cloudnest"`) {
					t.Error("401 的认证提示或 details 错误")
				}
			}
			if strings.Contains(response.Body.String(), raw) || strings.Contains(logs.String(), raw) || strings.Contains(logs.String(), "private-") {
				t.Fatal("响应或日志包含刷新原文")
			}
		})
	}
}
