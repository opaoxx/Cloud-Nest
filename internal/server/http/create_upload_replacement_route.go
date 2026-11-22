package http

import (
	"context"
	"encoding/json"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
)

// RegisterCreateUploadReplacementRoute 注册受 Bearer 授权保护的内容替换创建入口。
func RegisterCreateUploadReplacementRoute(
	router *gin.Engine,
	authorize func(context.Context, string) (*domain.Actor, error),
	create func(context.Context, domain.Actor, uint64, dto.CreateUploadReplacementInput, string) (json.RawMessage, error),
) {
	// 1. 绑定共享授权和已验证的替换创建处理函数。
	if router == nil {
		panic("注册内容替换创建接口需要路由器")
	}
	protected := middleware.RequireAuthorization(authorize, handler.AuthorizationFailure)
	createReplacement := handler.CreateUploadReplacement(create)

	// 2. 在正式 POST 路径依次执行授权与创建。
	router.POST("/api/v1/entries/:id/content-replacement", protected, createReplacement)
}
