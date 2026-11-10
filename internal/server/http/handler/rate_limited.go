package handler

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// RateLimited 返回统一 429；Retry-After 使用向上取整且至少为 1 的秒数。
func RateLimited(requestContext *gin.Context, retryAfter time.Duration) {
	// 1. 将等待时长向上取整为整数秒，不用浮点或可能溢出的加法。
	seconds := int64(retryAfter / time.Second)
	if retryAfter%time.Second > 0 {
		seconds++
	}
	if seconds < 1 {
		seconds = 1
	}

	// 2. 构造固定错误与请求 ID，设置 Retry-After 和禁止缓存。
	response := dto.ErrorResponse{
		Error:     dto.ErrorDetail{Code: "RATE_LIMITED", Message: "登录尝试过于频繁，请稍后重试", Details: nil},
		RequestID: requestcontext.RequestID(requestContext.Request.Context()),
	}
	requestContext.Header("Retry-After", strconv.FormatInt(seconds, 10))
	requestContext.Header("Cache-Control", "no-store")

	// 3. 返回 429 JSON，停止后续处理。
	requestContext.AbortWithStatusJSON(http.StatusTooManyRequests, response)
}
