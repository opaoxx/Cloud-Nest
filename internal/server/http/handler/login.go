package handler

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/security"
)

// Login 绑定已有来源策略、共享限流适配器、凭证结果及资料服务。
// authenticate 绑定 LoginAttemptGate.Authenticate，createTokens/getProfile 绑定对应服务方法。
func Login(
	policy *security.LoginSourcePolicy,
	authenticate func(*http.Request, string, string) (*domain.VerifiedAccount, time.Duration, error),
	createTokens func(context.Context, domain.VerifiedAccount, string, string, string) (*domain.LoginTokenResult, error),
	getProfile func(context.Context, uint64) (*domain.UserProfile, error),
	secureCookie bool,
) gin.HandlerFunc {
	// 1. 确认各项依赖在装配时已经提供。
	if policy == nil || authenticate == nil || createTokens == nil || getProfile == nil {
		panic("登录处理函数需要来源策略、限流凭证验证、令牌及资料服务")
	}
	return func(requestContext *gin.Context) {
		// 1. 读取 JSON，校验登录字段与客户端来源。
		requestContext.Header("Cache-Control", "no-store")
		requestContext.Header("Pragma", "no-cache")
		ctx := requestContext.Request.Context()
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		var input dto.LoginRequest
		if !binding.ReadJSON(requestContext, &input) {
			return
		}
		if validationError := input.Validate(); validationError != nil {
			InvalidArgument(requestContext, validationError)
			return
		}
		if !policy.Allows(requestContext.Request, input.ClientType) {
			ClientSourceForbidden(requestContext)
			return
		}

		// 2. 通过共享限流适配器验证账号凭证。
		account, retryAfter, err := authenticate(requestContext.Request, input.Username, input.Password)
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		if err != nil {
			AuthorizationFailure(requestContext, err)
			return
		}
		if retryAfter > 0 {
			if account != nil {
				InternalError(requestContext, nil)
				return
			}
			RateLimited(requestContext, retryAfter)
			return
		}
		if account == nil || account.ID == 0 || retryAfter < 0 {
			InternalError(requestContext, nil)
			return
		}

		// 3. 读取并检查已验证账号的用户资料。
		profile, err := getProfile(ctx, account.ID)
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		if err != nil {
			AuthorizationFailure(requestContext, err)
			return
		}
		if profile == nil || profile.ID != account.ID {
			InternalError(requestContext, nil)
			return
		}
		if _, err := dto.NewUserProfileData(profile); err != nil {
			InternalError(requestContext, nil)
			return
		}

		// 4. 创建会话和令牌，交付登录成功响应。
		result, err := createTokens(ctx, *account, input.InstallationID, input.DeviceName, input.Platform)
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		if err != nil {
			AuthorizationFailure(requestContext, err)
			return
		}
		LoginSuccess(requestContext, result, profile, input.ClientType, secureCookie)
	}
}
