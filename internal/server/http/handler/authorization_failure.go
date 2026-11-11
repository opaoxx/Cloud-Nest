package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/security"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// AuthorizationFailure 将认证失败映射为公开错误，内部故障沿用统一 500。
func AuthorizationFailure(requestContext *gin.Context, authorizationError error) {
	// 1. 按错误类别选择固定状态、业务码和提示。
	status := http.StatusUnauthorized
	code, message := "AUTH_REQUIRED", "请登录后重试"
	switch {
	case errors.Is(authorizationError, security.ErrInvalidAuthorizationHeader),
		errors.Is(authorizationError, domain.ErrInvalidAccessToken),
		errors.Is(authorizationError, domain.ErrAccountNotFound):
	case errors.Is(authorizationError, domain.ErrAccessTokenExpired):
		code, message = "TOKEN_EXPIRED", "访问令牌已过期，请刷新后重试"
	case errors.Is(authorizationError, domain.ErrSessionNotFound),
		errors.Is(authorizationError, domain.ErrSessionExpired),
		errors.Is(authorizationError, domain.ErrSessionRevoked),
		errors.Is(authorizationError, domain.ErrDeviceRevoked):
		code, message = "SESSION_REVOKED", "登录会话已失效，请重新登录"
	case errors.Is(authorizationError, domain.ErrAccountDisabled):
		status, code, message = http.StatusForbidden, "ACCOUNT_DISABLED", "账号已被禁用"
	default:
		InternalError(requestContext, nil)
		return
	}

	// 2. 构造带请求 ID 的错误响应，设置认证提示与禁止缓存。
	response := dto.ErrorResponse{
		Error:     dto.ErrorDetail{Code: code, Message: message, Details: nil},
		RequestID: requestcontext.RequestID(requestContext.Request.Context()),
	}
	if status == http.StatusUnauthorized {
		requestContext.Header("WWW-Authenticate", `Bearer realm="cloudnest"`)
	}
	requestContext.Header("Cache-Control", "no-store")

	// 3. 返回 JSON 并停止后续处理。
	requestContext.AbortWithStatusJSON(status, response)
}
