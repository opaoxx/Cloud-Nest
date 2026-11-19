package http

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

type uploadHandlerDeadlineWriter struct {
	gin.ResponseWriter
	setDeadline func(time.Time) error
}

func (writer uploadHandlerDeadlineWriter) SetReadDeadline(deadline time.Time) error {
	return writer.setDeadline(deadline)
}

func TestUploadContentHandlerOrder(t *testing.T) {
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	const path = "/test-upload-content/:id/content"
	calls := []string{"lookup", "deadline", "receive"}
	for _, fixture := range []struct {
		name   string
		status int
		code   string
		calls  int
	}{
		{"accepted", 202, "", 3},
		{"maximum task ID", 202, "", 3},
		{"missing bearer before bad path", 401, "AUTH_REQUIRED", 0},
		{"actor absent", 500, "INTERNAL_ERROR", 0},
		{"canceled before lookup", 500, "INTERNAL_ERROR", 0},
		{"bad path before bad media", 400, "INVALID_ARGUMENT", 0},
		{"bad media", 415, "UNSUPPORTED_MEDIA_TYPE", 0},
		{"missing task", 404, "UPLOAD_NOT_FOUND", 1},
		{"lookup cancels request", 500, "INTERNAL_ERROR", 1},
		{"foreign declaration", 500, "INTERNAL_ERROR", 1},
		{"wrong declaration ID", 500, "INTERNAL_ERROR", 1},
		{"missing declaration", 500, "INTERNAL_ERROR", 1},
		{"oversize declaration", 413, "FILE_TOO_LARGE", 1},
		{"known length mismatch", 413, "BODY_SIZE_MISMATCH", 1},
		{"invalid length", 400, "INVALID_ARGUMENT", 1},
		{"deadline setup failure", 500, "INTERNAL_ERROR", 2},
		{"receive busy", 409, "UPLOAD_BUSY", 3},
		{"receive session revoked", 401, "SESSION_REVOKED", 3},
		{"receive cancels request", 500, "INTERNAL_ERROR", 3},
		{"missing result", 500, "INTERNAL_ERROR", 3},
		{"wrong result ID", 500, "INTERNAL_ERROR", 3},
		{"foreign result", 500, "INTERNAL_ERROR", 3},
		{"wrong result phase", 500, "INTERNAL_ERROR", 3},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			actor := domain.Actor{UserID: 7, DeviceID: 15, SessionID: 17}
			task := uploadTaskSuccessFixture()
			task.FileSize = 4
			rawID := "19"
			if fixture.name == "maximum task ID" {
				task.ID, rawID = ^uint64(0), "18446744073709551615"
			}
			taskID := task.ID
			if fixture.name == "bad path before bad media" || fixture.name == "missing bearer before bad path" {
				rawID = "0"
			}
			body := &uploadContentTrackedBody{reader: strings.NewReader("abcd")}
			var order []string
			var lookupContext context.Context
			var readDeadline time.Time
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			getTask := func(ctx context.Context, current domain.Actor, id uint64) (*domain.UploadTask, error) {
				order = append(order, "lookup")
				lookupContext = ctx
				if current != actor || id != taskID || body.reads != 0 || requestcontext.RequestID(ctx) != requestID {
					t.Fatal("读取任务前身份、任务 ID 或正文所有权错误")
				}
				switch fixture.name {
				case "missing task":
					return nil, domain.ErrUploadTaskNotFound
				case "lookup cancels request":
					cancel()
				case "foreign declaration":
					task.UserID++
				case "wrong declaration ID":
					task.ID--
				case "missing declaration":
					return nil, nil
				case "oversize declaration":
					task.FileSize = domain.MaxSingleUploadSizeBytes + 1
				}
				return task, nil
			}
			receive := func(ctx context.Context, current domain.Actor, declared *domain.UploadTask, source io.Reader) (*domain.UploadTask, error) {
				order = append(order, "receive")
				deadline, ok := ctx.Deadline()
				if !ok || !deadline.Equal(readDeadline) || ctx == lookupContext || current != actor ||
					declared != task || source != body || body.reads != 0 || requestcontext.RequestID(ctx) != requestID {
					t.Fatal("接收服务没有收到带期限的上下文或原始任务/正文")
				}
				result := *task
				result.Status = domain.UploadStatusVerifying
				switch fixture.name {
				case "receive busy":
					return nil, domain.ErrUploadTaskBusy
				case "receive session revoked":
					return nil, domain.ErrSessionRevoked
				case "receive cancels request":
					cancel()
				case "missing result":
					return nil, nil
				case "wrong result ID":
					result.ID--
				case "foreign result":
					result.UserID++
				case "wrong result phase":
					result.Status = domain.UploadStatusUploading
				}
				return &result, nil
			}
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			handlers := []gin.HandlerFunc{}
			if fixture.name != "actor absent" {
				handlers = append(handlers, middleware.RequireAuthorization(func(context.Context, string) (*domain.Actor, error) {
					return &actor, nil
				}, handler.AuthorizationFailure))
			}
			handlers = append(handlers, func(ctx *gin.Context) {
				ctx.Writer = uploadHandlerDeadlineWriter{ctx.Writer, func(deadline time.Time) error {
					order = append(order, "deadline")
					readDeadline = deadline
					if fixture.name == "deadline setup failure" {
						return errors.New("private deadline failure")
					}
					return nil
				}}
				if fixture.name == "canceled before lookup" {
					cancel()
				}
			}, handler.UploadContent(getTask, receive), func(*gin.Context) { t.Error("响应后进入下游") })
			router.PUT(path, handlers...)
			request := httptest.NewRequest(http.MethodPut, "/test-upload-content/"+rawID+"/content?user_id=99&size_bytes=400", nil).WithContext(parent)
			request.Body, request.ContentLength = body, 4
			request.Header.Set("X-Request-ID", requestID)
			request.Header.Set("Content-Type", "application/octet-stream")
			if fixture.name != "missing bearer before bad path" {
				request.Header.Set("Authorization", "Bearer fixture-access")
			}
			switch fixture.name {
			case "bad path before bad media", "bad media":
				request.Header.Set("Content-Type", "application/json")
			case "known length mismatch":
				request.ContentLength = 3
			case "invalid length":
				request.ContentLength = -2
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != fixture.status || !slices.Equal(order, calls[:fixture.calls]) || body.reads != 0 || body.closes != 0 {
				t.Fatal("响应、调用顺序或正文所有权错误", response.Code, response.Body.String(), order)
			}
			if fixture.status == http.StatusAccepted {
				var value dto.UploadContentAcceptedResponse
				if json.Unmarshal(response.Body.Bytes(), &value) != nil || value.Data.TaskID != rawID ||
					value.Data.Status != "VERIFYING" || value.RequestID != requestID {
					t.Fatal("202 正文错误", response.Body.String())
				}
			} else {
				var value dto.ErrorResponse
				if json.Unmarshal(response.Body.Bytes(), &value) != nil || value.Error.Code != fixture.code || value.RequestID != requestID {
					t.Fatal("失败响应错误", response.Body.String())
				}
			}
		})
	}
}

func TestUploadContentHandlerRealHTTP(t *testing.T) {
	for _, fixture := range []struct {
		name    string
		payload string
		size    uint64
		chunked bool
		status  int
	}{
		{"known binary", "a\x00\xffb", 4, false, 202},
		{"chunked binary", "a\x00\xffb", 4, true, 202},
		{"empty", "", 0, false, 202},
		{"chunked short", "abc", 4, true, 413},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			task := uploadTaskSuccessFixture()
			task.FileSize = fixture.size
			actor := domain.Actor{UserID: task.UserID, DeviceID: 15, SessionID: 17}
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			authorize := middleware.RequireAuthorization(func(context.Context, string) (*domain.Actor, error) {
				return &actor, nil
			}, handler.AuthorizationFailure)
			router.PUT("/test-upload-content/:id/content", authorize, handler.UploadContent(
				func(context.Context, domain.Actor, uint64) (*domain.UploadTask, error) { return task, nil },
				func(ctx context.Context, _ domain.Actor, declared *domain.UploadTask, source io.Reader) (*domain.UploadTask, error) {
					var received strings.Builder
					if _, err := service.CopyUploadContent(ctx, &received, source, declared.FileSize); err != nil {
						return nil, err
					}
					if received.String() != fixture.payload {
						t.Error("接收回调未取得原始二进制正文")
					}
					result := *declared
					result.Status = domain.UploadStatusVerifying
					return &result, nil
				},
			))
			server := httptest.NewServer(router)
			defer server.Close()
			var source io.Reader = strings.NewReader(fixture.payload)
			if fixture.chunked {
				source = struct{ io.Reader }{source}
			}
			request, err := http.NewRequest(http.MethodPut, server.URL+"/test-upload-content/19/content", source)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Content-Type", "application/octet-stream")
			request.Header.Set("Authorization", "Bearer fixture-access")
			client := server.Client()
			client.Timeout = 3 * time.Second
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, readError := io.ReadAll(response.Body)
			closeError := response.Body.Close()
			if response.StatusCode != fixture.status || readError != nil || closeError != nil {
				t.Fatal("真实接收响应错误", response.StatusCode, string(body), readError, closeError)
			}
			if response.StatusCode == http.StatusAccepted {
				var value dto.UploadContentAcceptedResponse
				if json.Unmarshal(body, &value) != nil || value.Data.TaskID != "19" || value.Data.Status != "VERIFYING" ||
					value.RequestID == "" || value.RequestID != response.Header.Get("X-Request-ID") {
					t.Fatal("真实接收结果错误", string(body))
				}
			} else {
				var value dto.ErrorResponse
				if json.Unmarshal(body, &value) != nil || value.Error.Code != "BODY_SIZE_MISMATCH" {
					t.Fatal("实际大小不符响应错误", string(body))
				}
			}
		})
	}
}
