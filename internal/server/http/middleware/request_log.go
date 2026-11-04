package middleware

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// RequestLog 在请求处理完成后记录 ID、方法、URL 路径、最终状态及毫秒耗时。
// logger 由启动入口提供；本中间件应位于 RequestID 之后、Recovery 之前。
// 不记录查询参数、请求/响应正文、请求头、错误集合或原始 panic 载荷。
// 已恢复异常附带 recovered_panic=true 并使用 ERROR，status 仍保留实际值。
func RequestLog(logger *slog.Logger) gin.HandlerFunc {
	return func(requestContext *gin.Context) {
		// 1. 记录开始时间、请求方法和路径，不读取查询参数或正文。
		startedAt := time.Now()
		method := requestContext.Request.Method
		path := requestContext.Request.URL.Path

		// 2. 执行后续处理，等待正常响应或异常恢复完成。
		requestContext.Next()

		// 3. 根据最终状态码确定日志级别。
		statusCode := requestContext.Writer.Status()
		level := slog.LevelInfo
		recoveredPanic := requestContext.GetBool(recoveredPanicKey)
		if recoveredPanic || statusCode >= http.StatusInternalServerError {
			level = slog.LevelError
		} else if statusCode >= http.StatusBadRequest {
			level = slog.LevelWarn
		}

		// 4. 读取最终请求 ID，输出一条 JSON 完成日志。
		attributes := []any{
			"request_id", requestcontext.RequestID(requestContext.Request.Context()),
			"method", method,
			"path", path,
			"status", statusCode,
			"duration_ms", time.Since(startedAt).Seconds() * 1000,
		}
		if recoveredPanic {
			attributes = append(attributes, "recovered_panic", true)
		}
		logger.Log(requestContext.Request.Context(), level, "HTTP 请求完成", attributes...)
	}
}
