package middleware

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/security"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// RequireAuthorization 绑定授权服务与失败响应，供受保护路由显式挂载。
// authorize 成功后才能保存身份，reject 接收错误并负责公开响应映射。
func RequireAuthorization(
	authorize func(context.Context, string) (*domain.Actor, error),
	reject func(*gin.Context, error),
) gin.HandlerFunc {
	// 1. 构造时确认依赖完整，每次请求复用同一组能力。
	if authorize == nil || reject == nil {
		panic("认证中间件需要授权服务和失败响应方法")
	}
	return func(requestContext *gin.Context) {
		// 1. 检查请求上下文，提取 Bearer 凭证。
		ctx := requestContext.Request.Context()
		if err := ctx.Err(); err != nil {
			reject(requestContext, err)
			requestContext.Abort()
			return
		}
		tokenText, parseError := security.BearerToken(requestContext.Request)
		if parseError != nil {
			reject(requestContext, parseError)
			requestContext.Abort()
			return
		}

		// 2. 调用授权服务，处理错误并复查取消状态。
		actor, authorizationError := authorize(ctx, tokenText)
		if err := ctx.Err(); err != nil {
			authorizationError = err
		}
		if authorizationError != nil {
			reject(requestContext, authorizationError)
			requestContext.Abort()
			return
		}

		// 3. 检查授权结果，保存 Actor 到请求上下文。
		if actor == nil || actor.UserID == 0 || actor.SessionID == 0 || actor.DeviceID == 0 {
			reject(requestContext, domain.ErrInvalidSessionData)
			requestContext.Abort()
			return
		}
		ctx = requestcontext.WithActor(ctx, *actor)
		requestContext.Request = requestContext.Request.WithContext(ctx)

		// 4. 继续执行后续处理函数。
		requestContext.Next()
	}
}
