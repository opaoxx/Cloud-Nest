package binding

import (
	"mime"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const unsupportedUploadContentMessage = "请求必须使用 application/octet-stream，且不压缩正文"

// ValidateUploadContentHeaders 校验原始上传内容的媒体类型与内容编码。
// 供已认证内容处理函数在租约准备和正文读取前调用，失败时须立即 return。
func ValidateUploadContentHeaders(requestContext *gin.Context) bool {
	// 1. 检查当前处理状态，停止已结束的请求。
	if requestContext.Writer.Written() || requestContext.IsAborted() {
		requestContext.Abort()
		return false
	}

	// 2. 解析单个 Content-Type，核对二进制媒体类型与正文编码。
	request := requestContext.Request
	contentTypes := request.Header.Values("Content-Type")
	if len(contentTypes) != 1 {
		return abortUploadContentRequest(requestContext, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", unsupportedUploadContentMessage)
	}
	mediaType, _, parseError := mime.ParseMediaType(contentTypes[0])
	encodings := request.Header.Values("Content-Encoding")
	hasSupportedEncoding := len(encodings) == 0 || (len(encodings) == 1 && strings.EqualFold(strings.TrimSpace(encodings[0]), "identity"))
	if parseError != nil || mediaType != "application/octet-stream" || !hasSupportedEncoding {
		return abortUploadContentRequest(requestContext, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", unsupportedUploadContentMessage)
	}

	// 3. 格式通过后交给调用方继续处理文件流。
	return true
}

// abortUploadContentRequest 清理待发送正文头，并交付固定的原始内容输入错误。
func abortUploadContentRequest(requestContext *gin.Context, status int, code, message string) bool {
	// 1. 清理待发送正文头，保证错误 JSON 按自身长度及编码交付。
	requestContext.Writer.Header().Del("Content-Length")
	requestContext.Writer.Header().Del("Content-Encoding")
	requestContext.Header("Pragma", "no-cache")
	requestContext.Header("Content-Type", "application/json; charset=utf-8")
	// 2. 复用 JSON 与上传校验共用的输入错误交付。
	return abortRequest(requestContext, status, code, message)
}
