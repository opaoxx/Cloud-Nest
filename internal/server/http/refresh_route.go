package http

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/security"
)

// RegisterRefreshRoute 使用已有来源配置与刷新服务注册正式刷新路径。
func RegisterRefreshRoute(
	router *gin.Engine,
	allowedOrigins []string,
	refreshTokens func(context.Context, string) (*domain.RefreshTokenResult, error),
	secureCookie bool,
) {
	// 1. 检查路由器，构造来源策略与已有刷新处理函数。
	if router == nil {
		panic("注册刷新接口需要路由器")
	}
	policy := security.NewLoginSourcePolicy(allowedOrigins)
	refresh := handler.Refresh(policy, refreshTokens, secureCookie)
	// 2. 在公共中间件之后注册正式刷新路径。
	router.POST("/api/v1/auth/refresh", refresh)
}
