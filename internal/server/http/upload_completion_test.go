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

func TestUploadCompletionHandlerFlow(t *testing.T) {
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	const requestBody = `{"id":"99","user_id":"99"}private-body`
	for _, scenario := range []struct {
		name, path, code, message string
		httpStatus, calls         int
		taskID                    uint64
		confirmError              error
		changeResult              func(*domain.UploadTask)
		missingActor, nilResult   bool
		cancelOn                  int
		cancelAfter               bool
	}{
		{name: "verifying", httpStatus: 202, calls: 1},
		{name: "maximum task ID", path: "18446744073709551615", taskID: math.MaxUint64, httpStatus: 202, calls: 1},
		{name: "completed on another device", httpStatus: 200, calls: 1, changeResult: func(task *domain.UploadTask) {
			objectID, resultID := uint64(23), uint64(31)
			task.Status, task.ReservedBytes, task.DeviceID = domain.UploadStatusCompleted, 0, 15
			task.PublishedObjectID, task.ResultEntryID, task.CompletedAt = &objectID, &resultID, &task.UpdatedAt
		}},
		{name: "missing actor", path: "0", missingActor: true, httpStatus: 500, code: "INTERNAL_ERROR"},
		{name: "invalid task ID", path: "019", httpStatus: 400, code: "INVALID_ARGUMENT"},
		{name: "missing task", confirmError: fmt.Errorf("private-lookup: %w", domain.ErrUploadTaskNotFound), httpStatus: 404, calls: 1, code: "UPLOAD_NOT_FOUND", message: "上传任务不存在"},
		{name: "task not ready", confirmError: fmt.Errorf("private-lookup: %w", domain.ErrUploadTaskNotReady), httpStatus: 409, calls: 1, code: "TASK_NOT_READY", message: "上传任务当前不可完成"},
		{name: "unexpected busy error", confirmError: domain.ErrUploadTaskBusy, httpStatus: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "account disabled", confirmError: domain.ErrAccountDisabled, httpStatus: 403, calls: 1, code: "ACCOUNT_DISABLED"},
		{name: "session revoked", confirmError: domain.ErrSessionRevoked, httpStatus: 401, calls: 1, code: "SESSION_REVOKED"},
		{name: "unknown failure", confirmError: errors.New("private-database"), httpStatus: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "nil result", nilResult: true, httpStatus: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "wrong task", httpStatus: 500, calls: 1, code: "INTERNAL_ERROR", changeResult: func(task *domain.UploadTask) { task.ID++ }},
		{name: "foreign result", httpStatus: 500, calls: 1, code: "INTERNAL_ERROR", changeResult: func(task *domain.UploadTask) { task.UserID++ }},
		{name: "canceled before confirmation", cancelOn: 2, httpStatus: 500, code: "INTERNAL_ERROR"},
		{name: "canceled after confirmation", cancelAfter: true, httpStatus: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "cancel supersedes task error", cancelAfter: true, confirmError: domain.ErrUploadTaskNotReady, httpStatus: 500, calls: 1, code: "INTERNAL_ERROR"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 提供可信 Actor 和可控确认服务，核对传入参数。
			actor := domain.Actor{UserID: 7, SessionID: 17, DeviceID: 11}
			taskID := scenario.taskID
			if taskID == 0 {
				taskID = 19
			}
			task := uploadTaskSuccessFixture()
			task.ID, task.Status = taskID, domain.UploadStatusVerifying
			if scenario.changeResult != nil {
				scenario.changeResult(task)
			}
			ctx, stop := context.WithCancel(t.Context())
			defer stop()
			var confirmationContext context.Context
			calls, downstream := 0, 0
			confirm := handler.UploadCompletion(func(received context.Context, current domain.Actor, target uint64) (*domain.UploadTask, error) {
				calls++
				if received != confirmationContext || current != actor || target != taskID {
					t.Fatal("完成确认未收到当前请求上下文、可信 Actor 或路径任务 ID", current, target)
				}
				if scenario.cancelAfter {
					stop()
				}
				if scenario.nilResult {
					return nil, nil
				}
				return task, scenario.confirmError
			})
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.POST("/test-uploads/:id/complete", func(requestContext *gin.Context) {
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
				confirmationContext = requestContext.Request.Context()
				if !scenario.missingActor {
					confirmationContext = requestcontext.WithActor(confirmationContext, actor)
				}
				if scenario.cancelOn > 0 {
					confirmationContext = &cancelUserSessionsContext{Context: confirmationContext, cancel: stop, cancelOn: scenario.cancelOn}
				}
				requestContext.Request = requestContext.Request.WithContext(confirmationContext)
			}, confirm, func(*gin.Context) { downstream++ })
			path := scenario.path
			if path == "" {
				path = "19"
			}
			bodyReader := strings.NewReader(requestBody)
			request := httptest.NewRequest(http.MethodPost, "/test-uploads/"+path+"/complete?id=99&user_id=99", bodyReader).WithContext(ctx)
			request.Header.Set("X-Request-ID", strings.ToUpper(requestID))
			request.Header.Set("X-User-ID", "99")
			request.Header.Set("Idempotency-Key", "private-key")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			// 2. 核对状态码、调用次数、禁止缓存头及公开正文。
			if response.Code != scenario.httpStatus || calls != scenario.calls || downstream != 0 || bodyReader.Len() != len(requestBody) ||
				response.Header().Get("X-Request-ID") != requestID || response.Header().Get("Cache-Control") != "no-store" ||
				response.Header().Get("Pragma") != "no-cache" || response.Header().Get("Content-Type") != "application/json; charset=utf-8" ||
				response.Header().Get("Content-Encoding") != "" || strings.Contains(response.Body.String(), "private-") {
				t.Fatal("完成确认处理顺序、正文或响应头不符", response.Code, calls, downstream, response.Body.String(), response.Header())
			}
			if scenario.code == "" {
				var body dto.UploadTaskResponse
				if json.Unmarshal(response.Body.Bytes(), &body) != nil || body.RequestID != requestID || body.Data.ID != strconv.FormatUint(taskID, 10) ||
					body.Data.Status != string(task.Status) {
					t.Fatal("完成确认成功响应不符", body)
				}
				return
			}
			var failure dto.ErrorResponse
			if json.Unmarshal(response.Body.Bytes(), &failure) != nil || failure.RequestID != requestID || failure.Error.Code != scenario.code ||
				(scenario.message != "" && failure.Error.Message != scenario.message) || response.Header().Get("Content-Length") != "" {
				t.Fatal("完成确认错误响应不符", failure)
			}
			if scenario.httpStatus == http.StatusBadRequest {
				if len(failure.Error.Details) != 1 || failure.Error.Details["field"] != "id" {
					t.Fatal("路径字段错误提示不符", failure.Error.Details)
				}
			} else if failure.Error.Details != nil {
				t.Fatal("完成确认错误附带额外详情", failure.Error.Details)
			}
			if (scenario.httpStatus == http.StatusUnauthorized) != (response.Header().Get("WWW-Authenticate") == `Bearer realm="cloudnest"`) {
				t.Fatal("认证提示与状态不符", response.Header())
			}
		})
	}
}

func TestUploadCompletionHandlerRequiresDependency(t *testing.T) {
	// 1. 缺少确认服务时在装配阶段报告错误。
	defer func() {
		if recover() == nil {
			t.Error("缺少确认服务仍构造了 Handler")
		}
	}()
	handler.UploadCompletion(nil)
}
