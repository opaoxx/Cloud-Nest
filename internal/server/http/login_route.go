package http

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/security"
)

// RegisterLoginRoute 绑定已有来源配置及服务方法，注册真实登录路径。
// authenticate 须来自共享 LoginAttemptGate，确保不同请求共用失败额度。
func RegisterLoginRoute(
	router *gin.Engine,
	allowedOrigins []string,
	authenticate func(*http.Request, string, string) (*domain.VerifiedAccount, time.Duration, error),
	createTokens func(context.Context, domain.VerifiedAccount, string, string, string) (*domain.LoginTokenResult, error),
	getProfile func(context.Context, uint64) (*domain.UserProfile, error),
	secureCookie bool,
) {
	// 1. 检查路由器，构造来源策略与已有登录处理函数。
	if router == nil {
		panic("注册登录接口需要路由器")
	}
	policy := security.NewLoginSourcePolicy(allowedOrigins)
	login := handler.Login(policy, authenticate, createTokens, getProfile, secureCookie)
	// 2. 在公共中间件之后注册登录路径。
	router.POST("/api/v1/auth/login", login)
}
