package http

import (
	"context"
	"encoding/json"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
)

// RegisterRestoreEntryRoute 注册受 Bearer 授权保护的单项恢复路由。
func RegisterRestoreEntryRoute(
	router *gin.Engine,
	authorize func(context.Context, string) (*domain.Actor, error),
	restore func(context.Context, domain.Actor, uint64, uint64, string) (json.RawMessage, error),
) {
	// 1. 绑定共享认证和恢复处理函数。
	if router == nil {
		panic("注册目录项恢复接口需要路由器")
	}
	protected := middleware.RequireAuthorization(authorize, handler.AuthorizationFailure)
	restoreEntry := handler.RestoreEntry(restore)

	// 2. 在正式 POST 路径上依次执行认证和恢复处理。
	router.POST("/api/v1/trash/:id/restore", protected, restoreEntry)
}
