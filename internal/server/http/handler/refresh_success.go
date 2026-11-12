package handler

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// RefreshSuccess 交付已完成来源校验及刷新服务调用的成功结果。
// secureCookie 由可信启动配置提供，HTTP 开发环境为 false，HTTPS 部署为 true。
func RefreshSuccess(requestContext *gin.Context, result *domain.RefreshTokenResult, clientType string, secureCookie bool) {
	// 1. 检查响应状态和请求取消。
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

	// 2. 转换并预先编码刷新成功数据。
	data, err := dto.NewRefreshData(result, clientType, time.Now())
	if err != nil {
		InternalError(requestContext, nil)
		return
	}
	responseBytes, err := json.Marshal(dto.RefreshResponse{Data: *data, RequestID: requestcontext.RequestID(ctx)})
	if err != nil {
		InternalError(requestContext, nil)
		return
	}

	// 3. 按客户端模式准备并校验刷新 Cookie。
	var refreshCookie *http.Cookie
	if clientType == "web" {
		refreshCookie, err = newRefreshCookie(result.RefreshToken, result.Session.ExpiresAt, secureCookie)
		if err != nil {
			InternalError(requestContext, nil)
			return
		}
	}

	// 4. 复查取消，写入响应头、Cookie 和 JSON。
	if ctx.Err() != nil {
		InternalError(requestContext, nil)
		return
	}
	requestContext.Writer.Header().Del("Content-Length")
	requestContext.Writer.Header().Del("Content-Encoding")
	requestContext.Header("Content-Type", "application/json; charset=utf-8")
	if refreshCookie != nil {
		http.SetCookie(requestContext.Writer, refreshCookie)
	}
	requestContext.Abort()
	requestContext.Data(http.StatusOK, "application/json; charset=utf-8", responseBytes)
}
