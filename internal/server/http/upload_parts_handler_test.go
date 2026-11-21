package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

const uploadPartsHandlerTestPath = "/test-upload-parts"

func TestUploadPartsHandlerFlow(t *testing.T) {
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	const credential = "private-test-credential"
	const partSize = 16 << 20
	const checksum = "TgdAhWK+24tgzgXB3s/jrRa3IjCWfeAfZAt+Rym0n84="
	actor := domain.Actor{UserID: 7, DeviceID: 19, SessionID: 17}
	for _, test := range []struct {
		name, path, code, field    string
		status, calls              int
		taskID                     uint64
		noBearer, deniedOrigin     bool
		empty, invalidResult       bool
		cancelBefore, cancelDuring bool
		serviceError               error
	}{
		{name: "parts with gaps", status: 200, calls: 1},
		{name: "empty array and maximum ID", status: 200, calls: 1, empty: true, taskID: math.MaxUint64},
		{name: "authentication before path", status: 401, code: "AUTH_REQUIRED", path: "0", noBearer: true},
		{name: "blocked origin", status: 403, code: "CORS_FORBIDDEN", deniedOrigin: true},
		{name: "invalid path", status: 400, code: "INVALID_ARGUMENT", field: "id", path: "019"},
		{name: "missing or hidden task", status: 404, calls: 1, code: "UPLOAD_NOT_FOUND",
			serviceError: fmt.Errorf("private-task: %w", domain.ErrUploadTaskNotFound)},
		{name: "task no longer readable", status: 409, calls: 1, code: "TASK_NOT_READY", serviceError: domain.ErrUploadTaskNotReady},
		{name: "invalid persisted plan", status: 409, calls: 1, code: "TASK_NOT_READY", serviceError: domain.ErrInvalidUploadTaskData},
		{name: "storage unavailable", status: 503, calls: 1, code: "STORAGE_UNAVAILABLE",
			serviceError: fmt.Errorf("private-storage: %w", domain.ErrUploadStorageUnavailable)},
		{name: "missing upload session", status: 503, calls: 1, code: "STORAGE_UNAVAILABLE", serviceError: domain.ErrUploadPartSessionMissing},
		{name: "account disabled", status: 403, calls: 1, code: "ACCOUNT_DISABLED", serviceError: domain.ErrAccountDisabled},
		{name: "session revoked", status: 401, calls: 1, code: "SESSION_REVOKED", serviceError: domain.ErrSessionRevoked},
		{name: "internal failure", status: 500, calls: 1, code: "INTERNAL_ERROR", serviceError: errors.New("private-database")},
		{name: "invalid receipts", status: 500, calls: 1, code: "INTERNAL_ERROR", invalidResult: true},
		{name: "cancel before service", status: 500, code: "INTERNAL_ERROR", cancelBefore: true},
		{name: "cancel during service", status: 500, calls: 1, code: "INTERNAL_ERROR", cancelDuring: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			taskID := test.taskID
			if taskID == 0 {
				taskID = 19
			}
			result := &domain.UploadPartReceiptList{PartSizeBytes: partSize, PartCount: 3, Parts: []domain.UploadPartReceipt{
				{PartNumber: 1, SizeBytes: partSize, ETag: "First", ChecksumSHA256: checksum},
				{PartNumber: 3, SizeBytes: 1, ETag: "Tail", ChecksumSHA256: checksum},
			}}
			if test.empty {
				result.Parts = nil
			}
			if test.invalidResult {
				result.Parts[1].ETag = `"private-etag"`
			}

			// 1. 挂载真实认证中间件及测试服务，记录可信调用参数。
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			var authorizedContext context.Context
			var capturedLog bytes.Buffer
			router := NewRouter(logging.New(&capturedLog), nil, func(context.Context) bool { return true })
			auth := middleware.RequireAuthorization(func(_ context.Context, token string) (*domain.Actor, error) {
				if token != credential {
					t.Error("认证收到的测试凭证不符")
				}
				return &actor, nil
			}, handler.AuthorizationFailure)
			calls := 0
			list := handler.UploadParts(func(ctx context.Context, received domain.Actor, id uint64) (*domain.UploadPartReceiptList, error) {
				calls++
				if ctx != authorizedContext || requestcontext.RequestID(ctx) != requestID || received != actor || id != taskID {
					t.Error("回执服务未收到可信身份、原上下文和完整任务 ID")
				}
				if test.cancelDuring {
					cancel()
				}
				return result, test.serviceError
			})
			router.GET(uploadPartsHandlerTestPath+"/:id", auth, func(requestContext *gin.Context) {
				authorizedContext = requestContext.Request.Context()
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
				if test.cancelBefore {
					cancel()
				}
				list(requestContext)
			}, func(*gin.Context) { t.Error("交付回执响应后仍执行下游") })

			// 2. 使用合成请求核对状态、响应头、公开字段及固定错误。
			path := test.path
			if path == "" {
				path = strconv.FormatUint(taskID, 10)
			}
			request := httptest.NewRequest(http.MethodGet, uploadPartsHandlerTestPath+"/"+path+"?user_id=99&trace=private-query", nil).WithContext(parent)
			request.Header.Set("X-Request-ID", strings.ToUpper(requestID))
			request.Header.Set("X-User-ID", "99")
			if !test.noBearer {
				request.Header.Set("Authorization", "Bearer "+credential)
			}
			if test.deniedOrigin {
				request.Header.Set("Origin", "https://private-origin.example.invalid")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			headers := response.Result().Header
			if response.Code != test.status || calls != test.calls || headers.Get("Cache-Control") != "no-store" ||
				headers.Get("X-Request-ID") != requestID || headers.Get("Content-Encoding") != "" || headers.Get("Content-Length") == "3" ||
				(headers.Get("WWW-Authenticate") != "") != (test.status == 401) || strings.Contains(capturedLog.String(), "private-") {
				t.Fatal("回执处理状态、调用次数、响应头或日志保护不符", response.Code, calls)
			}
			if test.status == http.StatusOK {
				var success dto.UploadPartsResponse
				want, err := dto.NewUploadPartsData(result)
				if err != nil || json.Unmarshal(response.Body.Bytes(), &success) != nil || success.RequestID != requestID ||
					!reflect.DeepEqual(success.Data, *want) || headers.Get("Pragma") != "no-cache" ||
					headers.Get("Content-Type") != "application/json; charset=utf-8" {
					t.Fatal("公开回执清单、数组或缓存响应头不符", err)
				}
				return
			}
			var details map[string]any
			if test.field != "" {
				details = map[string]any{"field": test.field}
			}
			var failure dto.ErrorResponse
			if json.Unmarshal(response.Body.Bytes(), &failure) != nil || failure.RequestID != requestID || failure.Error.Code != test.code ||
				!reflect.DeepEqual(failure.Error.Details, details) || strings.Contains(response.Body.String(), "private-") ||
				strings.Contains(response.Body.String(), `"data"`) {
				t.Fatal("错误响应偏离契约或包含部分回执", failure.Error.Code)
			}
		})
	}
}

func TestUploadPartsHandlerPreservesCommittedResponse(t *testing.T) {
	router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
	list := handler.UploadParts(func(context.Context, domain.Actor, uint64) (*domain.UploadPartReceiptList, error) {
		t.Error("已写出响应后仍调用了回执服务")
		return nil, nil
	})
	router.GET(uploadPartsHandlerTestPath+"/:id", func(requestContext *gin.Context) {
		requestContext.String(http.StatusAccepted, "already-written")
	}, list, func(*gin.Context) { t.Error("已提交响应仍执行下游") })
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, uploadPartsHandlerTestPath+"/19", nil))
	if response.Code != http.StatusAccepted || response.Body.String() != "already-written" || response.Header().Get("Cache-Control") != "" {
		t.Fatal("已提交的响应被改写")
	}
}
