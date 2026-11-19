package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

func TestUploadCancellationHandlerFlow(t *testing.T) {
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	const requestBody = `{"id":"99","user_id":"99"}private-body`
	for _, scenario := range []struct {
		name, path, code, message string
		httpStatus, calls         int
		taskID                    uint64
		beginError                error
		changeResult              func(*domain.UploadTask)
		missingActor, nilResult   bool
		cancelOn                  int
		cancelAfter               bool
	}{
		{name: "accepted", httpStatus: 202, calls: 1},
		{name: "maximum task ID", path: "18446744073709551615", taskID: math.MaxUint64, httpStatus: 202, calls: 1},
		{name: "same user other device", httpStatus: 202, calls: 1, changeResult: func(task *domain.UploadTask) { task.DeviceID = 15 }},
		{name: "terminal replay", httpStatus: 200, calls: 1, changeResult: func(task *domain.UploadTask) {
			task.Status, task.ReservedBytes, task.CompletedAt = domain.UploadStatusCancelled, 0, &task.UpdatedAt
		}},
		{name: "missing actor", path: "0", missingActor: true, httpStatus: 500, code: "INTERNAL_ERROR"},
		{name: "zero ID", path: "0", httpStatus: 400, code: "INVALID_ARGUMENT"},
		{name: "leading zero", path: "019", httpStatus: 400, code: "INVALID_ARGUMENT"},
		{name: "overflow ID", path: "18446744073709551616", httpStatus: 400, code: "INVALID_ARGUMENT"},
		{name: "missing task", beginError: fmt.Errorf("private-transaction: %w", domain.ErrUploadTaskNotFound), httpStatus: 404, calls: 1, code: "UPLOAD_NOT_FOUND", message: "上传任务不存在"},
		{name: "live lease", beginError: fmt.Errorf("private-transaction: %w", domain.ErrUploadTaskBusy), httpStatus: 409, calls: 1, code: "UPLOAD_BUSY", message: "上传任务正在处理中"},
		{name: "task not ready", beginError: domain.ErrUploadTaskNotReady, httpStatus: 409, calls: 1, code: "TASK_NOT_READY", message: "上传任务当前不可取消"},
		{name: "session revoked in transaction", beginError: domain.ErrSessionRevoked, httpStatus: 401, calls: 1, code: "SESSION_REVOKED"},
		{name: "device revoked in transaction", beginError: domain.ErrDeviceRevoked, httpStatus: 401, calls: 1, code: "SESSION_REVOKED"},
		{name: "account disabled in transaction", beginError: domain.ErrAccountDisabled, httpStatus: 403, calls: 1, code: "ACCOUNT_DISABLED"},
		{name: "unknown database failure", beginError: errors.New("private-transaction"), httpStatus: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "nil result", nilResult: true, httpStatus: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "wrong task", httpStatus: 500, calls: 1, code: "INTERNAL_ERROR", changeResult: func(task *domain.UploadTask) { task.ID++ }},
		{name: "foreign result", httpStatus: 500, calls: 1, code: "INTERNAL_ERROR", changeResult: func(task *domain.UploadTask) { task.UserID++ }},
		{name: "unexpected pending stage", httpStatus: 500, calls: 1, code: "INTERNAL_ERROR", changeResult: func(task *domain.UploadTask) {
			task.Status = domain.UploadStatusExpiring
		}},
		{name: "canceled at entry", cancelOn: 1, httpStatus: 500, code: "INTERNAL_ERROR"},
		{name: "canceled before write", cancelOn: 2, httpStatus: 500, code: "INTERNAL_ERROR"},
		{name: "canceled after acceptance", cancelAfter: true, httpStatus: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "cancel supersedes task error", cancelAfter: true, beginError: domain.ErrUploadTaskNotFound, httpStatus: 500, calls: 1, code: "INTERNAL_ERROR"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 准备可信身份和受理结果，核对 Handler 交给事务的参数。
			actor := domain.Actor{UserID: 7, SessionID: 17, DeviceID: 11}
			taskID := scenario.taskID
			if taskID == 0 {
				taskID = 19
			}
			task := uploadTaskSuccessFixture()
			task.ID, task.Status = taskID, domain.UploadStatusCancelling
			if scenario.changeResult != nil {
				scenario.changeResult(task)
			}
			ctx, stop := context.WithCancel(t.Context())
			defer stop()
			var transactionContext context.Context
			calls, downstream := 0, 0
			cancelUpload := handler.UploadCancellation(func(received context.Context, identity domain.Actor, target uint64) (*domain.UploadTask, error) {
				calls++
				if received != transactionContext || identity != actor || target != taskID {
					t.Fatal("受理未收到原上下文、可信身份或路径任务 ID", identity, target)
				}
				if scenario.cancelAfter {
					stop()
				}
				if scenario.nilResult || scenario.beginError != nil {
					return nil, scenario.beginError
				}
				return task, nil
			})
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.POST("/test-uploads/:id/cancel", func(requestContext *gin.Context) {
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
				requestContext.Header("Content-Type", "application/octet-stream")
				transactionContext = requestContext.Request.Context()
				if !scenario.missingActor {
					transactionContext = requestcontext.WithActor(transactionContext, actor)
				}
				if scenario.cancelOn > 0 {
					transactionContext = &cancelUserSessionsContext{Context: transactionContext, cancel: stop, cancelOn: scenario.cancelOn}
				}
				requestContext.Request = requestContext.Request.WithContext(transactionContext)
			}, cancelUpload, func(*gin.Context) { downstream++ })
			path := scenario.path
			if path == "" {
				path = "19"
			}
			bodyReader := strings.NewReader(requestBody)
			request := httptest.NewRequest(http.MethodPost, "/test-uploads/"+path+"/cancel?id=99&user_id=99", bodyReader).WithContext(ctx)
			request.Header.Set("X-Request-ID", strings.ToUpper(requestID))
			request.Header.Set("X-User-ID", "99")
			request.Header.Set("Idempotency-Key", "private-key")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			// 2. 核对公开响应、安全响应头、调用次数和未读取的请求体。
			if response.Code != scenario.httpStatus || calls != scenario.calls || downstream != 0 || bodyReader.Len() != len(requestBody) ||
				response.Header().Get("X-Request-ID") != requestID || response.Header().Get("Cache-Control") != "no-store" ||
				response.Header().Get("Pragma") != "no-cache" || response.Header().Get("Content-Type") != "application/json; charset=utf-8" ||
				response.Header().Get("Content-Encoding") != "" || strings.Contains(response.Body.String(), "private-") {
				t.Fatal("取消处理顺序、响应体或响应头不符", response.Code, calls, downstream, response.Body.String(), response.Header())
			}
			if scenario.code == "" {
				var body dto.UploadTaskResponse
				if json.Unmarshal(response.Body.Bytes(), &body) != nil || body.RequestID != requestID || body.Data.ID != strconv.FormatUint(taskID, 10) ||
					body.Data.Status != string(task.Status) || response.Header().Get("Content-Length") != strconv.Itoa(response.Body.Len()) {
					t.Fatal("任务取消成功响应不符", body)
				}
				return
			}
			var body dto.ErrorResponse
			if json.Unmarshal(response.Body.Bytes(), &body) != nil || body.RequestID != requestID || body.Error.Code != scenario.code ||
				(scenario.message != "" && body.Error.Message != scenario.message) || response.Header().Get("Content-Length") != "" {
				t.Fatal("任务取消错误响应不符", body)
			}
			if scenario.httpStatus == http.StatusBadRequest {
				if len(body.Error.Details) != 1 || body.Error.Details["field"] != "id" {
					t.Fatal("任务 ID 字段错误不符", body.Error.Details)
				}
			} else if body.Error.Details != nil {
				t.Fatal("任务取消错误包含额外详情", body.Error.Details)
			}
			if (scenario.httpStatus == http.StatusUnauthorized) != (response.Header().Get("WWW-Authenticate") == `Bearer realm="cloudnest"`) {
				t.Fatal("认证提示与状态不符", response.Header())
			}
		})
	}
}

func TestUploadCancellationHandlerEndedResponse(t *testing.T) {
	for _, writeBody := range []bool{true, false} {
		// 1. 模拟已发送或主动终止，由 Handler 保留原响应。
		router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
		cancelUpload := handler.UploadCancellation(func(context.Context, domain.Actor, uint64) (*domain.UploadTask, error) {
			t.Fatal("响应已结束仍调用取消受理")
			return nil, nil
		})
		wantBody := ""
		if writeBody {
			wantBody = "existing body"
		}
		router.POST("/test-uploads/:id/cancel", func(requestContext *gin.Context) {
			requestContext.Header("Cache-Control", "private")
			if writeBody {
				requestContext.String(http.StatusConflict, wantBody)
			} else {
				requestContext.AbortWithStatus(http.StatusConflict)
			}
			cancelUpload(requestContext)
		}, func(*gin.Context) { t.Error("响应结束后继续执行下游") })
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/test-uploads/19/cancel", nil))
		if response.Code != http.StatusConflict || response.Body.String() != wantBody || response.Header().Get("Cache-Control") != "private" {
			t.Fatal("取消 Handler 改写了已结束的响应", response.Code, response.Body.String(), response.Header())
		}
	}
}

func TestUploadCancellationHandlerRequiresDependency(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("缺少受理方法时仍完成了装配")
		}
	}()
	handler.UploadCancellation(nil)
}
