package http

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
)

func TestUploadContentFailureDelivery(t *testing.T) {
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	const internalMessage = "服务内部错误，请稍后重试"
	for _, fixture := range []struct {
		name    string
		err     error
		request error
		status  int
		code    string
		message string
	}{
		{"missing task", domain.ErrUploadTaskNotFound, nil, 404, "UPLOAD_NOT_FOUND", "上传任务不存在"},
		{"task not ready", domain.ErrUploadTaskNotReady, nil, 409, "TASK_NOT_READY", "上传任务当前不可接收文件内容"},
		{"lease version exhausted", domain.ErrUploadLeaseVersionExhausted, nil, 409, "TASK_NOT_READY", "上传任务当前不可接收文件内容"},
		{"lease busy", domain.ErrUploadTaskBusy, nil, 409, "UPLOAD_BUSY", "上传任务正在处理中"},
		{"storage object already exists", domain.ErrUploadObjectExists, nil, 409, "UPLOAD_BUSY", "上传任务正在处理中"},
		{"receiver limit", domain.ErrUploadReceiverBusy, nil, 409, "UPLOAD_BUSY", "上传接收繁忙，请稍后重试"},
		{"file too large", domain.ErrUploadFileTooLarge, nil, 413, "FILE_TOO_LARGE", "文件大小不能超过 16 MiB"},
		{"body size mismatch", domain.ErrUploadBodySizeMismatch, nil, 413, "BODY_SIZE_MISMATCH", "文件实际大小与声明不一致"},
		{"checksum mismatch", domain.ErrUploadChecksumMismatch, nil, 422, "CHECKSUM_MISMATCH", "文件校验失败，请重新上传"},
		{"temporary capacity", domain.ErrUploadTempSpaceExhausted, nil, 507, "TEMP_SPACE_EXHAUSTED", "上传临时空间不足，请稍后重试"},
		{"storage unavailable", domain.ErrUploadStorageUnavailable, nil, 503, "STORAGE_UNAVAILABLE", "存储服务暂时不可用"},
		{"renewal cause with child cancellation", errors.Join(context.Canceled, domain.ErrUploadTaskBusy), nil, 409, "UPLOAD_BUSY", "上传任务正在处理中"},
		{"transfer timeout wins over read cancellation", errors.Join(binding.ErrUploadTransferTimeout, context.Canceled), context.Canceled, 408, "UPLOAD_TIMEOUT", "上传传输超时，请查询任务状态后重试"},
		{"request deadline", domain.ErrUploadTaskBusy, context.DeadlineExceeded, 408, "UPLOAD_TIMEOUT", "上传传输超时，请查询任务状态后重试"},
		{"request cancellation", domain.ErrUploadTaskBusy, context.Canceled, 500, "INTERNAL_ERROR", internalMessage},
		{"dependency deadline", context.DeadlineExceeded, nil, 500, "INTERNAL_ERROR", internalMessage},
		{"device revoked", domain.ErrDeviceRevoked, nil, 401, "SESSION_REVOKED", "登录会话已失效，请重新登录"},
		{"session expired", domain.ErrSessionExpired, nil, 401, "SESSION_REVOKED", "登录会话已失效，请重新登录"},
		{"account disabled", domain.ErrAccountDisabled, nil, 403, "ACCOUNT_DISABLED", "账号已被禁用"},
		{"task record invalid", domain.ErrInvalidUploadTaskData, nil, 500, "INTERNAL_ERROR", internalMessage},
		{"unclassified error", errors.New("unknown fixture error"), nil, 500, "INTERNAL_ERROR", internalMessage},
		{"nil error", nil, nil, 500, "INTERNAL_ERROR", internalMessage},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			receiveError := fixture.err
			if receiveError != nil {
				receiveError = fmt.Errorf("private storage path and diagnostic: %w", receiveError)
			}
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.PUT("/test-upload-content-failure", func(requestContext *gin.Context) {
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
				requestContext.Header("Content-Type", "application/octet-stream")
				ctx := requestContext.Request.Context()
				switch fixture.request {
				case context.Canceled:
					canceled, stop := context.WithCancel(ctx)
					stop()
					requestContext.Request = requestContext.Request.WithContext(canceled)
				case context.DeadlineExceeded:
					expired, stop := context.WithDeadline(ctx, time.Now().Add(-time.Second))
					defer stop()
					requestContext.Request = requestContext.Request.WithContext(expired)
				}
				handler.UploadContentFailure(requestContext, receiveError)
			}, func(*gin.Context) { t.Error("失败交付后进入下游") })
			request := httptest.NewRequest(http.MethodPut, "/test-upload-content-failure", nil)
			request.Header.Set("X-Request-ID", requestID)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			want := fmt.Sprintf(`{"error":{"code":%q,"message":%q,"details":null},"request_id":%q}`, fixture.code, fixture.message, requestID)
			if response.Code != fixture.status || response.Body.String() != want {
				t.Fatal("公开错误状态或正文不符", response.Code, response.Body.String())
			}
			headers := response.Header()
			wantChallenge := ""
			if fixture.status == http.StatusUnauthorized {
				wantChallenge = `Bearer realm="cloudnest"`
			}
			if headers.Get("X-Request-ID") != requestID || headers.Get("Cache-Control") != "no-store" ||
				headers.Get("Pragma") != "no-cache" || headers.Get("Content-Type") != "application/json; charset=utf-8" ||
				headers.Get("Content-Length") == "3" || headers.Get("Content-Encoding") != "" || headers.Get("WWW-Authenticate") != wantChallenge {
				t.Fatal("公开错误响应头不符", headers)
			}
		})
	}
}

func TestUploadContentFailureEndedResponse(t *testing.T) {
	for _, written := range []bool{true, false} {
		router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
		router.PUT("/test-upload-content-failure", func(requestContext *gin.Context) {
			requestContext.Header("Cache-Control", "private")
			if written {
				requestContext.String(http.StatusAccepted, "existing body")
			} else {
				requestContext.Status(http.StatusAccepted)
				requestContext.Abort()
			}
			handler.UploadContentFailure(requestContext, domain.ErrUploadTaskBusy)
		}, func(*gin.Context) { t.Error("处理结束后进入下游") })
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/test-upload-content-failure", nil))
		wantBody := ""
		if written {
			wantBody = "existing body"
		}
		if response.Code != http.StatusAccepted || response.Body.String() != wantBody ||
			response.Header().Get("Cache-Control") != "private" || response.Header().Get("Pragma") != "" {
			t.Fatal("已结束的响应被覆盖", response.Code, response.Body.String())
		}
	}
}
