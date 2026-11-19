package middleware

import (
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// FinishUnreadUploadBody 在 HTTP/1.x 上传响应交付时收尾尚未读完的正文。
// 在可能提前结束请求的上传中间件和处理函数之前挂载。
func FinishUnreadUploadBody(requestContext *gin.Context) {
	// 1. 跟踪 HTTP/1.x 上传正文是否读到结束。
	request := requestContext.Request
	if request.ProtoMajor != 1 || request.Body == nil || request.Body == http.NoBody || request.ContentLength == 0 || requestContext.Writer.Written() {
		requestContext.Next()
		return
	}
	body := &uploadBodyReader{ReadCloser: request.Body}
	request.Body = body

	// 2. 在响应提交前，为未读完的正文设置连接收尾。
	writer := &uploadBodyResponseWriter{ResponseWriter: requestContext.Writer, body: body}
	requestContext.Writer = writer

	// 3. 覆盖只选择状态、尚未提交响应以及 panic 展开的路径。
	defer writer.finishIfUnread()
	requestContext.Next()
}

// uploadBodyReader 只记录调用方正常读到结束的事实，关闭仍沿用原正文。
type uploadBodyReader struct {
	io.ReadCloser
	reachedEOF bool
}

func (body *uploadBodyReader) Read(buffer []byte) (int, error) {
	count, err := body.ReadCloser.Read(buffer)
	if err == io.EOF {
		body.reachedEOF = true
	}
	return count, err
}

// uploadBodyResponseWriter 在 Gin 实际提交响应头前执行一次收尾。
type uploadBodyResponseWriter struct {
	gin.ResponseWriter
	body     *uploadBodyReader
	finished bool
}

func (writer *uploadBodyResponseWriter) finishIfUnread() {
	// 1. 已完成读取或已经提交的响应沿用原连接行为。
	if writer.finished || writer.body.reachedEOF || writer.Written() {
		return
	}
	writer.finished = true

	// 2. 告知 HTTP/1.x 先发响应并关闭此连接，再限制服务端关闭正文时的自动读取。
	writer.Header().Set("Connection", "close")
	_ = http.NewResponseController(writer.ResponseWriter).SetReadDeadline(time.Now())
}

// Unwrap 允许传输期限助手继续访问底层 HTTP 写入器。
func (writer *uploadBodyResponseWriter) Unwrap() http.ResponseWriter {
	return writer.ResponseWriter
}

func (writer *uploadBodyResponseWriter) WriteHeaderNow() {
	writer.finishIfUnread()
	writer.ResponseWriter.WriteHeaderNow()
}

func (writer *uploadBodyResponseWriter) Write(data []byte) (int, error) {
	writer.finishIfUnread()
	return writer.ResponseWriter.Write(data)
}

func (writer *uploadBodyResponseWriter) WriteString(value string) (int, error) {
	writer.finishIfUnread()
	return writer.ResponseWriter.WriteString(value)
}

func (writer *uploadBodyResponseWriter) Flush() {
	writer.finishIfUnread()
	writer.ResponseWriter.Flush()
}
