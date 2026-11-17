package binding

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// ValidateUploadContentLength 核对已解析的请求长度与当前任务的声明大小。
// declaredSize 来自已核对归属和上传能力的任务，未知长度交给后续实际字节校验。
func ValidateUploadContentLength(requestContext *gin.Context, declaredSize uint64) bool {
	// 1. 检查当前处理状态，停止已结束的请求。
	if requestContext.Writer.Written() || requestContext.IsAborted() {
		requestContext.Abort()
		return false
	}

	// 2. 核对解析后的长度元数据，允许未知长度。
	contentLength := requestContext.Request.ContentLength
	if contentLength < -1 {
		return abortUploadContentRequest(requestContext, http.StatusBadRequest, "INVALID_ARGUMENT", "请求正文长度无效")
	}
	if contentLength == -1 {
		return true
	}

	// 3. 比较已知长度与任务声明，不一致时结束处理。
	if uint64(contentLength) != declaredSize {
		return abortUploadContentRequest(requestContext, http.StatusRequestEntityTooLarge, "BODY_SIZE_MISMATCH", "请求正文长度与上传声明不一致")
	}

	// 4. 校验通过，交给后续实际字节读取。
	return true
}
