package middleware

import "github.com/gin-gonic/gin"

// recoveredPanicKey 仅在 HTTP 中间件间传递异常布尔标记，不存储原始载荷。
const recoveredPanicKey = "cloudnest.recovered_panic"

// Recovery 保留 Gin 的恢复和断连处理，禁止原始转储，并标记交给回调处理的异常。
// handleRecovery 由路由层提供，完成日志可据标记识别响应提交后的异常。
func Recovery(handleRecovery gin.RecoveryFunc) gin.HandlerFunc {
	// 1. 关闭原始转储，只记录布尔标记，再调用路由层提供的响应处理方法。
	return gin.CustomRecoveryWithWriter(nil, func(requestContext *gin.Context, recovered any) {
		requestContext.Set(recoveredPanicKey, true)
		handleRecovery(requestContext, recovered)
	})
}
