package http

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestUploadTransferDeadlineRealHTTP(t *testing.T) {
	for _, fixture := range []struct {
		name        string
		content     string
		stallUse    bool
		wantStatus  int
		wantUseCall bool
	}{
		{"known body stalls", "Content-Length: 4\r\n\r\nab", false, http.StatusRequestTimeout, false},
		{"chunked terminator stalls", "Transfer-Encoding: chunked\r\n\r\n4\r\nabcd\r\n", false, http.StatusRequestTimeout, false},
		{"storage stalls after full body", "Content-Length: 4\r\n\r\nabcd", true, http.StatusRequestTimeout, true},
		{"complete keeps connection reusable", "Content-Length: 4\r\n\r\nabcd", false, http.StatusNoContent, true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			directory := t.TempDir()
			files, err := service.NewUploadContentFileService(directory, service.NewUploadTempSpace())
			if err != nil {
				t.Fatal(err)
			}
			type outcome struct {
				err      error
				use      bool
				reads    int
				deadline time.Time
			}
			outcomes := make(chan outcome, 1)
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.PUT("/test-upload-deadline", func(requestContext *gin.Context) {
				if !binding.ValidateUploadContentHeaders(requestContext) || !binding.ValidateUploadContentLength(requestContext, 4) {
					return
				}
				parent, cancel := context.WithTimeout(requestContext.Request.Context(), 300*time.Millisecond)
				defer cancel()
				deadline, _ := parent.Deadline()
				useCalled := false
				source := &uploadContentTrackedBody{reader: requestContext.Request.Body}
				err := binding.WithUploadTransferDeadline(parent, requestContext.Writer, func(ctx context.Context) error {
					return files.WithFile(ctx, source, 4, nil, func(file io.ReadSeeker, _ domain.UploadContentDigest) error {
						useCalled = true
						payload, err := io.ReadAll(file)
						if err != nil || string(payload) != "abcd" {
							t.Error("传给文件使用回调的正文有误", err)
						}
						if fixture.stallUse {
							<-ctx.Done()
							return ctx.Err()
						}
						return nil
					})
				})
				outcomes <- outcome{err, useCalled, source.reads, deadline}
				switch {
				case errors.Is(err, context.DeadlineExceeded):
					requestContext.Status(http.StatusRequestTimeout)
				case err != nil:
					requestContext.Status(http.StatusInternalServerError)
				default:
					requestContext.Status(http.StatusNoContent)
				}
			})
			server := httptest.NewServer(router)
			defer server.Close()
			connection, err := net.DialTimeout("tcp", server.Listener.Addr().String(), 2*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			if err := connection.SetDeadline(time.Now().Add(4 * time.Second)); err != nil {
				t.Fatal(err)
			}
			// 保持客户端连接打开。缺失数据的请求只能依靠服务端读取期限退出。
			if _, err := fmt.Fprintf(connection, "PUT /test-upload-deadline HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/octet-stream\r\n%s", fixture.content); err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(connection)
			readStatus := func(method string) int {
				t.Helper()
				response, err := http.ReadResponse(reader, &http.Request{Method: method})
				if err != nil {
					t.Fatal("服务器未及时交付响应", err)
				}
				_, readError := io.Copy(io.Discard, response.Body)
				if closeError := response.Body.Close(); readError != nil || closeError != nil {
					t.Fatal("读取测试响应失败", readError, closeError)
				}
				return response.StatusCode
			}
			if status := readStatus(http.MethodPut); status != fixture.wantStatus {
				t.Fatal("上传传输状态错误", status)
			}
			result := <-outcomes
			if result.reads < 2 || result.use != fixture.wantUseCall || (fixture.wantStatus == http.StatusNoContent && result.err != nil) ||
				(fixture.wantStatus == http.StatusRequestTimeout && !errors.Is(result.err, context.DeadlineExceeded)) {
				t.Fatal("传输结果或文件使用时机错误", result.err, result.use)
			}
			entries, err := os.ReadDir(directory)
			if err != nil || len(entries) != 0 {
				t.Fatal("上传结束后临时文件未回收", err)
			}
			if fixture.wantStatus == http.StatusNoContent {
				// 等前一条请求的期限过去，再在同一连接验证后续请求正常。
				time.Sleep(time.Until(result.deadline.Add(20 * time.Millisecond)))
				if _, err := io.WriteString(connection, "GET /ping HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
					t.Fatal(err)
				}
				if status := readStatus(http.MethodGet); status != http.StatusOK {
					t.Fatal("上一条上传的期限影响了复用连接", status)
				}
			}
		})
	}
}
