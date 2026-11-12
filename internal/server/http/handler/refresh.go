package handler

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/security"
)

// Refresh 绑定认证来源策略与刷新凭证服务，按客户端模式交付结果。
// refreshTokens 绑定 RefreshTokenService.Refresh，secureCookie 由可信配置提供。
func Refresh(
	policy *security.LoginSourcePolicy,
	refreshTokens func(context.Context, string) (*domain.RefreshTokenResult, error),
	secureCookie bool,
) gin.HandlerFunc {
	// 1. 检查装配依赖，再创建每次请求的处理函数。
	if policy == nil || refreshTokens == nil {
		panic("刷新处理函数需要来源策略与刷新凭证服务")
	}
	return func(requestContext *gin.Context) {
		// 1. 检查请求状态，读取 JSON 并校验字段。
		if requestContext.Writer.Written() {
			requestContext.Abort()
			return
		}
		requestContext.Header("Cache-Control", "no-store")
		requestContext.Header("Pragma", "no-cache")
		ctx := requestContext.Request.Context()
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		var input dto.RefreshRequest
		if !binding.ReadJSON(requestContext, &input) {
			return
		}
		if validationError := input.Validate(); validationError != nil {
			InvalidArgument(requestContext, validationError)
			return
		}

		// 2. 核对客户端来源并提取刷新原文。
		if !policy.Allows(requestContext.Request, input.ClientType) {
			ClientSourceForbidden(requestContext)
			return
		}
		rawToken, err := security.RefreshToken(requestContext.Request, input)
		if err != nil {
			AuthorizationFailure(requestContext, err)
			return
		}

		// 3. 调用刷新服务，处理取消与服务错误。
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		result, err := refreshTokens(ctx, rawToken)
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		if err != nil {
			AuthorizationFailure(requestContext, err)
			return
		}

		// 4. 交付刷新成功响应。
		RefreshSuccess(requestContext, result, input.ClientType, secureCookie)
	}
}
