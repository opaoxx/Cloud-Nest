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

// LogoutSuccess 交付已完成认证、来源校验及退出事务提交的成功结果。
// clientType 须已校验，secureCookie 由可信启动配置提供。
func LogoutSuccess(requestContext *gin.Context, result *domain.LogoutResult, clientType string, secureCookie bool) {
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

	// 2. 校验客户端类型，转换并预先编码退出数据。
	if clientType != "web" && clientType != "native" {
		InternalError(requestContext, nil)
		return
	}
	data, err := dto.NewLogoutData(result)
	if err != nil {
		InternalError(requestContext, nil)
		return
	}
	responseBytes, err := json.Marshal(dto.LogoutResponse{Data: *data, RequestID: requestcontext.RequestID(ctx)})
	if err != nil {
		InternalError(requestContext, nil)
		return
	}

	// 3. 按客户端模式准备刷新 Cookie 清理。
	var refreshCookie *http.Cookie
	if clientType == "web" {
		// 复用原 Cookie 的名称、路径和安全属性，以过去期限及 Max-Age=0 删除。
		refreshCookie, err = newRefreshCookie("", time.Unix(1, 0), secureCookie)
		if err != nil {
			InternalError(requestContext, nil)
			return
		}
		// net/http 将负数 MaxAge 编码为响应头中的 Max-Age=0。
		refreshCookie.MaxAge = -1
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
