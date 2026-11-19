package http

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestFinishUnreadUploadBodyRejectsPromptly(t *testing.T) {
	for _, fixture := range []struct {
		name   string
		status int
		code   string
	}{
		{"missing authorization", 401, "AUTH_REQUIRED"},
		{"invalid path", 400, "INVALID_ARGUMENT"},
		{"unsupported media", 415, "UNSUPPORTED_MEDIA_TYPE"},
		{"wrong declared length", 413, "BODY_SIZE_MISMATCH"},
		{"expect continue rejected", 409, "UPLOAD_BUSY"},
		{"partially read chunked body", 409, "UPLOAD_BUSY"},
		{"status only", 400, ""},
		{"flush", 400, ""},
		{"panicked before response", 500, "INTERNAL_ERROR"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			authorize := middleware.RequireAuthorization(func(context.Context, string) (*domain.Actor, error) {
				return &actor, nil
			}, handler.AuthorizationFailure)
			router.PUT("/test-upload-finish/:id/content", middleware.FinishUnreadUploadBody, authorize, func(ctx *gin.Context) {
				switch fixture.name {
				case "status only", "flush":
					ctx.Status(http.StatusBadRequest)
					if fixture.name == "flush" {
						ctx.Writer.Flush()
					}
					ctx.Abort()
				case "panicked before response":
					panic("fixture upload panic")
				}
			}, handler.UploadContent(
				func(context.Context, domain.Actor, uint64) (*domain.UploadTask, error) {
					if fixture.name != "wrong declared length" && fixture.name != "expect continue rejected" && fixture.name != "partially read chunked body" {
						t.Error("提前拒绝时仍查询了任务")
					}
					return &domain.UploadTask{ID: 19, UserID: actor.UserID, FileSize: 4}, nil
				},
				func(_ context.Context, _ domain.Actor, _ *domain.UploadTask, source io.Reader) (*domain.UploadTask, error) {
					switch fixture.name {
					case "expect continue rejected":
					case "partially read chunked body":
						var firstByte [1]byte
						if _, err := io.ReadFull(source, firstByte[:]); err != nil || firstByte[0] != 'a' {
							t.Error("未收到部分正文", err)
						}
					default:
						t.Error("提前拒绝时仍调用接收流程")
					}
					return nil, domain.ErrUploadTaskBusy
				},
			))
			server := httptest.NewServer(router)
			defer server.Close()
			connection := openUploadBodyTestConnection(t, server)
			defer connection.Close()
			id, authorization, media := "19", "Authorization: Bearer fixture-access\r\n", "application/octet-stream"
			extra := "Content-Length: 4\r\n\r\nab"
			switch fixture.name {
			case "missing authorization":
				authorization = ""
			case "invalid path":
				id = "0"
			case "unsupported media":
				media = "application/json"
			case "wrong declared length":
				extra = "Content-Length: 3\r\n\r\nab"
			case "expect continue rejected":
				extra = "Expect: 100-continue\r\nContent-Length: 4\r\n\r\n"
			case "partially read chunked body":
				extra = "Transfer-Encoding: chunked\r\n\r\n2\r\nab\r\n"
			}
			if _, err := fmt.Fprintf(connection, "PUT /test-upload-finish/%s/content HTTP/1.1\r\nHost: localhost\r\n%sContent-Type: %s\r\n%s", id, authorization, media, extra); err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(connection)
			response, body := readUploadBodyTestResponse(t, reader, http.MethodPut)
			if response.StatusCode != fixture.status || !response.Close {
				t.Fatal("未交付预期错误或未要求关闭连接", response.StatusCode, response.Close, string(body))
			}
			if fixture.code != "" {
				var value dto.ErrorResponse
				if json.Unmarshal(body, &value) != nil || value.Error.Code != fixture.code ||
					value.RequestID == "" || value.RequestID != response.Header.Get("X-Request-ID") {
					t.Fatal("提前拒绝改变了公开错误格式", string(body))
				}
			} else if len(body) != 0 {
				t.Fatal("只返回状态时意外写入正文", string(body))
			}
			if _, err := reader.ReadByte(); err != io.EOF {
				t.Fatal("错误响应已返回，但连接没有及时收尾", err)
			}
		})
	}
}

func TestFinishUnreadUploadBodyKeepsCompleteConnection(t *testing.T) {
	for _, fixture := range []struct {
		name    string
		content string
		size    uint64
	}{
		{"known length", "Content-Length: 4\r\n\r\nabcd", 4},
		{"chunked", "Transfer-Encoding: chunked\r\n\r\n4\r\nabcd\r\n0\r\n\r\n", 4},
		{"empty", "Content-Length: 0\r\n\r\n", 0},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			authorize := middleware.RequireAuthorization(func(context.Context, string) (*domain.Actor, error) {
				return &domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}, nil
			}, handler.AuthorizationFailure)
			router.PUT("/test-upload-finish/:id/content", middleware.FinishUnreadUploadBody, authorize, handler.UploadContent(
				func(context.Context, domain.Actor, uint64) (*domain.UploadTask, error) {
					return &domain.UploadTask{ID: 19, UserID: 7, FileSize: fixture.size}, nil
				},
				func(ctx context.Context, _ domain.Actor, task *domain.UploadTask, source io.Reader) (*domain.UploadTask, error) {
					if _, err := service.CopyUploadContent(ctx, io.Discard, source, task.FileSize); err != nil {
						return nil, err
					}
					result := *task
					result.Status = domain.UploadStatusVerifying
					return &result, nil
				},
			))
			server := httptest.NewServer(router)
			defer server.Close()
			connection := openUploadBodyTestConnection(t, server)
			defer connection.Close()
			if _, err := io.WriteString(connection, "PUT /test-upload-finish/19/content HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer fixture-access\r\nContent-Type: application/octet-stream\r\n"+fixture.content); err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(connection)
			response, body := readUploadBodyTestResponse(t, reader, http.MethodPut)
			if response.StatusCode != http.StatusAccepted || response.Close || !strings.Contains(string(body), `"status":"VERIFYING"`) {
				t.Fatal("完整上传没有保持正常成功行为", response.StatusCode, response.Close, string(body))
			}
			if _, err := io.WriteString(connection, "GET /ping HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			response, _ = readUploadBodyTestResponse(t, reader, http.MethodGet)
			if response.StatusCode != http.StatusOK {
				t.Fatal("后续请求无法复用连接", response.StatusCode)
			}
		})
	}
}

func TestFinishUnreadUploadBodyLeavesHTTP2StreamToServer(t *testing.T) {
	router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
	router.PUT("/test-upload-finish", middleware.FinishUnreadUploadBody, func(ctx *gin.Context) {
		ctx.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "fixture rejection"})
	})
	server := httptest.NewUnstartedServer(router)
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	request, err := http.NewRequest(http.MethodPut, server.URL+"/test-upload-finish", strings.NewReader("abcd"))
	if err != nil {
		t.Fatal(err)
	}
	client := server.Client()
	client.Timeout = 3 * time.Second
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.ProtoMajor != 2 || response.StatusCode != http.StatusBadRequest ||
		response.Close || response.Header.Get("Connection") != "" {
		t.Fatal("HTTP/2 流受到了 HTTP/1.x 连接收尾的影响", response.Proto, response.StatusCode, response.Header)
	}
}

func openUploadBodyTestConnection(t *testing.T, server *httptest.Server) net.Conn {
	t.Helper()
	connection, err := net.DialTimeout("tcp", server.Listener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		_ = connection.Close()
		t.Fatal(err)
	}
	return connection
}

func readUploadBodyTestResponse(t *testing.T, reader *bufio.Reader, method string) (*http.Response, []byte) {
	t.Helper()
	response, err := http.ReadResponse(reader, &http.Request{Method: method})
	if err != nil {
		t.Fatal("服务器未及时交付响应", err)
	}
	body, readError := io.ReadAll(response.Body)
	closeError := response.Body.Close()
	if readError != nil || closeError != nil {
		t.Fatal("未读完整响应", readError, closeError)
	}
	return response, body
}
