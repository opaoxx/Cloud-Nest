package handler

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/security"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// Logout 绑定来源策略和退出服务，路由须先挂载认证中间件。
// logout 绑定 LogoutService.Logout，secureCookie 由可信启动配置提供。
func Logout(
	policy *security.LoginSourcePolicy,
	logout func(context.Context, domain.Actor) (*domain.LogoutResult, error),
	secureCookie bool,
) gin.HandlerFunc {
	// 1. 检查装配依赖，再创建每次请求的处理函数。
	if policy == nil || logout == nil {
		panic("退出处理函数需要来源策略与退出服务")
	}
	return func(requestContext *gin.Context) {
		// 1. 检查请求状态，读取已认证 Actor。
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
		actor, exists := requestcontext.Actor(ctx)
		if !exists {
			InternalError(requestContext, nil)
			return
		}

		// 2. 读取 JSON 并校验退出字段。
		var input dto.LogoutRequest
		if !binding.ReadJSON(requestContext, &input) {
			return
		}
		if validationError := input.Validate(); validationError != nil {
			InvalidArgument(requestContext, validationError)
			return
		}

		// 3. 核对客户端声明与请求来源。
		if !policy.Allows(requestContext.Request, input.ClientType) {
			ClientSourceForbidden(requestContext)
			return
		}

		// 4. 调用退出服务，处理取消与服务错误。
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		result, err := logout(ctx, actor)
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		if err != nil {
			AuthorizationFailure(requestContext, err)
			return
		}

		// 5. 交付退出成功响应。
		LogoutSuccess(requestContext, result, input.ClientType, secureCookie)
	}
}
