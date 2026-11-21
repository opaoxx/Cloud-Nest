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
	"time"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

const uploadPartURLsHandlerTestPath = "/test-part-urls"

func TestUploadPartURLsHandlerFlow(t *testing.T) {
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	const credential = "private-test-credential"
	firstDigest, secondDigest := strings.Repeat("00", 32), strings.Repeat("AB", 32)
	partsBody := fmt.Sprintf(`{"parts":[{"part_number":2,"sha256":%q},{"part_number":1,"sha256":%q}]`, firstDigest, secondDigest)
	oldExpiry := time.Date(2026, 11, 21, 12, 0, 0, 123456000, time.UTC)
	batchExpiry := oldExpiry.Add(24 * time.Hour)
	firstChecksum := strings.Repeat("A", 43) + "="
	const secondChecksum = "q6urq6urq6urq6urq6urq6urq6urq6urq6urq6urq6s="
	actor := domain.Actor{UserID: 7, DeviceID: 19, SessionID: 17}
	for _, test := range []struct {
		name, code, field, path, body string
		taskID                        uint64
		status, calls                 int
		noBearer, deniedOrigin        bool
		renew, wrongType, oversized   bool
		cancelBefore, cancelDuring    bool
		serviceError                  error
		changeBatch                   func(*domain.UploadPartURLBatch) *domain.UploadPartURLBatch
	}{
		{name: "ordinary", status: 200, calls: 1},
		{name: "explicit renewal", status: 200, calls: 1, renew: true, taskID: math.MaxUint64},
		{name: "authorization before path and JSON", status: 401, code: "AUTH_REQUIRED", noBearer: true, path: "0", body: `{"private-payload":`},
		{name: "origin rejected before signer", status: 403, code: "CORS_FORBIDDEN", deniedOrigin: true},
		{name: "path before JSON", status: 400, code: "INVALID_ARGUMENT", field: "id", path: "0", body: `{"private-payload":`},
		{name: "invalid JSON structure", status: 400, code: "INVALID_ARGUMENT", body: `{"private-payload":`},
		{name: "body cannot specify actor", status: 400, code: "INVALID_ARGUMENT", body: partsBody + `,"user_id":99}`},
		{name: "invalid number", status: 400, code: "INVALID_ARGUMENT", field: "parts.0.part_number",
			body: `{"parts":[{"part_number":0,"sha256":"private-payload"}]}`},
		{name: "invalid digest", status: 400, code: "INVALID_ARGUMENT", field: "parts.0.sha256",
			body: `{"parts":[{"part_number":2,"sha256":"private-payload"}]}`},
		{name: "invalid expiry", status: 400, code: "INVALID_ARGUMENT", field: "renew_if_expires_at",
			body: partsBody + `,"renew_if_expires_at":null}`},
		{name: "unsupported type", status: 415, code: "UNSUPPORTED_MEDIA_TYPE", wrongType: true},
		{name: "oversized", status: 413, code: "REQUEST_BODY_TOO_LARGE", oversized: true},
		{name: "outside task part plan", status: 400, calls: 1, code: "INVALID_ARGUMENT", field: "parts.0.part_number",
			serviceError: fmt.Errorf("private-payload: %w", &domain.UploadPartDeclarationError{
				Field: "parts.0.part_number", Message: domain.ErrInvalidUploadPartNumber.Error()})},
		{name: "missing or foreign task", status: 404, calls: 1, code: "UPLOAD_NOT_FOUND",
			serviceError: fmt.Errorf("private-payload: %w", domain.ErrUploadTaskNotFound)},
		{name: "stale renewal", status: 409, calls: 1, code: "TASK_NOT_READY", renew: true, serviceError: domain.ErrUploadTaskNotReady},
		{name: "busy", status: 409, calls: 1, code: "UPLOAD_BUSY", serviceError: domain.ErrUploadTaskBusy},
		{name: "storage unavailable", status: 503, calls: 1, code: "STORAGE_UNAVAILABLE", renew: true,
			serviceError: fmt.Errorf("private-payload: %w", domain.ErrUploadStorageUnavailable)},
		{name: "quota exceeded", status: 507, calls: 1, code: "QUOTA_EXCEEDED", renew: true, serviceError: domain.ErrQuotaExceeded},
		{name: "account disabled", status: 403, calls: 1, code: "ACCOUNT_DISABLED", serviceError: domain.ErrAccountDisabled},
		{name: "session revoked", status: 401, calls: 1, code: "SESSION_REVOKED", serviceError: domain.ErrSessionRevoked},
		{name: "internal failure", status: 500, calls: 1, code: "INTERNAL_ERROR", serviceError: errors.New("private-payload")},
		{name: "missing batch", status: 500, calls: 1, code: "INTERNAL_ERROR", changeBatch: func(*domain.UploadPartURLBatch) *domain.UploadPartURLBatch { return nil }},
		{name: "partial batch", status: 500, calls: 1, code: "INTERNAL_ERROR",
			changeBatch: func(batch *domain.UploadPartURLBatch) *domain.UploadPartURLBatch {
				batch.Parts = batch.Parts[:1]
				return batch
			}},
		{name: "reordered batch", status: 500, calls: 1, code: "INTERNAL_ERROR",
			changeBatch: func(batch *domain.UploadPartURLBatch) *domain.UploadPartURLBatch {
				batch.Parts[0], batch.Parts[1] = batch.Parts[1], batch.Parts[0]
				return batch
			}},
		{name: "different valid checksum", status: 500, calls: 1, code: "INTERNAL_ERROR",
			changeBatch: func(batch *domain.UploadPartURLBatch) *domain.UploadPartURLBatch {
				batch.Parts[0].ChecksumSHA256 = batch.Parts[1].ChecksumSHA256
				return batch
			}},
		{name: "cancel before signer", status: 500, code: "INTERNAL_ERROR", cancelBefore: true},
		{name: "cancel after signer started", status: 500, calls: 1, code: "INTERNAL_ERROR", cancelDuring: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			taskID := test.taskID
			if taskID == 0 {
				taskID = 23
			}
			batch := &domain.UploadPartURLBatch{ExpiresAt: batchExpiry, Parts: []domain.UploadPartURL{
				{PartNumber: 2, SizeBytes: 1, ChecksumSHA256: firstChecksum, URLExpiresAt: batchExpiry.Add(-time.Minute),
					URL: "https://objects.example.invalid/chunk%2B.bin?partNumber=2&X-Amz-Signature=private-signature"},
				{PartNumber: 1, SizeBytes: 16 << 20, ChecksumSHA256: secondChecksum, URLExpiresAt: batchExpiry.Add(-time.Minute),
					URL: "https://objects.example.invalid/chunk%2B.bin?partNumber=1&X-Amz-Signature=private-signature"},
			}}
			if test.changeBatch != nil {
				batch = test.changeBatch(batch)
			}
			// 1. 用共享认证中间件形成可信身份，检查传给签发服务的参数。
			requestParent, cancel := context.WithCancel(t.Context())
			defer cancel()
			var authorizedContext context.Context
			var capturedLog bytes.Buffer
			router := NewRouter(logging.New(&capturedLog), nil, func(context.Context) bool { return true })
			authorization := middleware.RequireAuthorization(func(_ context.Context, token string) (*domain.Actor, error) {
				if token != credential {
					t.Error("认证中间件收到的测试凭证不符")
				}
				return &actor, nil
			}, handler.AuthorizationFailure)
			calls := 0
			issue := handler.UploadPartURLs(func(ctx context.Context, received domain.Actor, id uint64, parts []domain.UploadPartDeclaration, expected *time.Time) (*domain.UploadPartURLBatch, error) {
				calls++
				wantParts := []domain.UploadPartDeclaration{{PartNumber: 2, SHA256: firstDigest}, {PartNumber: 1, SHA256: secondDigest}}
				if ctx != authorizedContext || requestcontext.RequestID(ctx) != requestID || received != actor || id != taskID || !reflect.DeepEqual(parts, wantParts) ||
					(test.renew && (expected == nil || *expected != oldExpiry)) || (!test.renew && expected != nil) {
					t.Error("签发服务未收到原请求身份、路径、声明和可选期限")
				}
				if test.cancelDuring {
					cancel()
				}
				return batch, test.serviceError
			})
			router.POST(uploadPartURLsHandlerTestPath+"/:id", authorization, func(requestContext *gin.Context) {
				authorizedContext = requestContext.Request.Context()
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
				if test.cancelBefore {
					cancel()
				}
				issue(requestContext)
			}, func(*gin.Context) { t.Error("交付签发结果后仍执行下游") })

			// 2. 发出合成请求，检查状态、固定错误和完整成功批次。
			path, body := test.path, test.body
			if path == "" {
				path = strconv.FormatUint(taskID, 10)
			}
			if body == "" {
				body = partsBody + "}"
				if test.renew {
					body = partsBody + fmt.Sprintf(`,"renew_if_expires_at":%q}`, oldExpiry.Format(time.RFC3339Nano))
				}
			}
			if test.oversized {
				body = strings.Repeat(" ", int(binding.MaxJSONBodyBytes)) + body
			}
			request := httptest.NewRequest(http.MethodPost, uploadPartURLsHandlerTestPath+"/"+path+"?user_id=99&trace=private-query", strings.NewReader(body)).WithContext(requestParent)
			request.Header.Set("X-Request-ID", strings.ToUpper(requestID))
			request.Header.Set("X-User-ID", "99")
			request.Header.Set("Content-Type", "application/json")
			if test.wrongType {
				request.Header.Set("Content-Type", "text/plain")
			}
			if test.deniedOrigin {
				request.Header.Set("Origin", "https://private-origin.example.invalid")
			}
			if !test.noBearer {
				request.Header.Set("Authorization", "Bearer "+credential)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			headers := response.Result().Header
			if response.Code != test.status || calls != test.calls || headers.Get("Cache-Control") != "no-store" ||
				headers.Get("X-Request-ID") != requestID || headers.Get("Content-Encoding") != "" || headers.Get("Content-Length") == "3" ||
				headers.Get("Set-Cookie") != "" || (headers.Get("WWW-Authenticate") != "") != (test.status == 401) ||
				strings.Contains(capturedLog.String(), "private-") {
				t.Fatal("状态、调用次数、响应头或日志保护不符", response.Code, calls)
			}
			if test.status == http.StatusOK {
				var success dto.UploadPartURLsResponse
				expected, err := dto.NewUploadPartURLsData(batch)
				if err != nil || json.Unmarshal(response.Body.Bytes(), &success) != nil || success.RequestID != requestID ||
					!reflect.DeepEqual(success.Data, *expected) || headers.Get("Pragma") != "no-cache" ||
					headers.Get("Content-Type") != "application/json; charset=utf-8" {
					t.Fatal("成功响应的有效期、签名正文或禁用缓存头不符", err)
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
				t.Fatal("错误响应不符或含有部分签名结果", failure.Error.Code)
			}
		})
	}
}

func TestUploadPartURLsHandlerPreservesCommittedResponse(t *testing.T) {
	router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
	issue := handler.UploadPartURLs(func(context.Context, domain.Actor, uint64, []domain.UploadPartDeclaration, *time.Time) (*domain.UploadPartURLBatch, error) {
		t.Error("已写出响应后仍调用了签发服务")
		return nil, nil
	})
	router.POST(uploadPartURLsHandlerTestPath+"/:id", func(requestContext *gin.Context) {
		requestContext.String(http.StatusAccepted, "already-written")
	}, issue, func(*gin.Context) { t.Error("已提交响应仍执行下游") })
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, uploadPartURLsHandlerTestPath+"/23", nil))
	if response.Code != http.StatusAccepted || response.Body.String() != "already-written" || response.Header().Get("Cache-Control") != "" {
		t.Fatal("已提交的响应被改写")
	}
}
