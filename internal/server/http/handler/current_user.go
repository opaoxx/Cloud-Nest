package handler

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// CurrentUser 绑定 UserProfileService.Get，返回当前已授权用户的资料。
// 路由须先执行认证中间件，缺少 Actor 作为装配异常处理。
func CurrentUser(getProfile func(context.Context, uint64) (*domain.UserProfile, error)) gin.HandlerFunc {
	// 1. 确认资料服务方法已经提供。
	if getProfile == nil {
		panic("当前用户处理函数需要资料服务方法")
	}
	return func(requestContext *gin.Context) {
		// 1. 检查请求上下文，读取已授权 Actor。
		requestContext.Header("Cache-Control", "no-store")
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

		// 2. 用 Actor 的用户 ID 调用资料服务，处理失败。
		profile, err := getProfile(ctx, actor.UserID)
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		if err != nil {
			AuthorizationFailure(requestContext, err)
			return
		}
		if profile == nil || profile.ID != actor.UserID {
			InternalError(requestContext, nil)
			return
		}

		// 3. 转换 DTO，返回带请求 ID 的资料响应。
		data, err := dto.NewUserProfileData(profile)
		if err != nil {
			InternalError(requestContext, nil)
			return
		}
		requestContext.JSON(http.StatusOK, dto.UserProfileResponse{
			Data:      *data,
			RequestID: requestcontext.RequestID(ctx),
		})
	}
}
