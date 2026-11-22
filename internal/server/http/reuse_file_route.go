package http

import (
	"context"
	"crypto/sha256"
	"encoding/json"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
)

// RegisterReuseFileRoute 将同账号秒传注册为受 Bearer 授权保护的正式入口。
func RegisterReuseFileRoute(
	router *gin.Engine,
	authorize func(context.Context, string) (*domain.Actor, error),
	reuse func(context.Context, domain.Actor, uint64, string, uint64, [sha256.Size]byte, string) (json.RawMessage, error),
) {
	// 1. 检查路由器，构造共享认证与秒传处理链。
	if router == nil {
		panic("注册秒传接口需要路由器")
	}
	protected := middleware.RequireAuthorization(authorize, handler.AuthorizationFailure)
	reuseFile := handler.ReuseFile(reuse)

	// 2. 将处理链挂载到正式路径。
	router.POST("/api/v1/files/reuse", protected, reuseFile)
}
