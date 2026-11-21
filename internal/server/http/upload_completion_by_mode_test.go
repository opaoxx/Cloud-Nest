package http

import (
	"context"
	"encoding/json"
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
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

func TestUploadCompletionByMode(t *testing.T) {
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	const jsonType = "application/json"
	const validBody = `{"parts":[{"part_number":2,"etag":"Tail"},{"part_number":1,"etag":"First"}]}`
	const badBody = `{"parts":[private-fixture`
	actor := domain.Actor{UserID: 7, DeviceID: 31, SessionID: 41}
	claims := []domain.UploadPartCompletionClaim{{PartNumber: 2, ETag: "Tail"}, {PartNumber: 1, ETag: "First"}}
	for _, test := range []struct {
		name, mode, body, contentType, path, code, details string
		status                                             domain.UploadStatus
		httpStatus, reads, completions, bodyCalls          int
		taskID                                             uint64
		lookupError, completionError                       error
		consume, noBearer, wrongOwner, wrongResult         bool
		streamedEmpty                                      bool
		cancelAfterLookup, cancelAfterComplete             bool
	}{
		{name: "single verifying ignores malformed payload", mode: "SINGLE", body: badBody, status: domain.UploadStatusVerifying, httpStatus: 202, reads: 1},
		{name: "single completed ignores multipart JSON", mode: "SINGLE", body: validBody, contentType: jsonType,
			status: domain.UploadStatusCompleted, httpStatus: 200, reads: 1},
		{name: "single uploading", mode: "SINGLE", status: domain.UploadStatusUploading, httpStatus: 409, reads: 1, code: "TASK_NOT_READY"},
		{name: "multipart new submission", mode: "MULTIPART", body: validBody, contentType: jsonType, status: domain.UploadStatusUploading,
			httpStatus: 202, reads: 1, completions: 1, bodyCalls: 1, consume: true, path: "18446744073709551615", taskID: math.MaxUint64},
		{name: "multipart completing ignores malformed payload", mode: "MULTIPART", body: badBody, status: domain.UploadStatusCompleting,
			httpStatus: 202, reads: 1, completions: 1},
		{name: "multipart verifying omits body", mode: "MULTIPART", status: domain.UploadStatusVerifying, httpStatus: 202, reads: 1, completions: 1},
		{name: "multipart completed omits body", mode: "MULTIPART", status: domain.UploadStatusCompleted, httpStatus: 200, reads: 1, completions: 1},
		{name: "multipart new submission requires body", mode: "MULTIPART", status: domain.UploadStatusUploading, httpStatus: 400,
			reads: 1, completions: 1, bodyCalls: 1, code: "INVALID_ARGUMENT", details: `{"field":"parts"}`},
		{name: "multipart empty streamed body", mode: "MULTIPART", status: domain.UploadStatusUploading, httpStatus: 400,
			contentType: jsonType, streamedEmpty: true, reads: 1, completions: 1, bodyCalls: 1,
			code: "INVALID_ARGUMENT", details: `{"field":"parts"}`},
		{name: "multipart duplicate numbers", mode: "MULTIPART", status: domain.UploadStatusUploading, contentType: jsonType,
			body: `{"parts":[{"part_number":1,"etag":"First"},{"part_number":1,"etag":"Other"}]}`, httpStatus: 400,
			reads: 1, completions: 1, bodyCalls: 1, consume: true, code: "INVALID_ARGUMENT", details: `{"field":"parts"}`},
		{name: "multipart ETag field", mode: "MULTIPART", status: domain.UploadStatusUploading, contentType: jsonType,
			body: `{"parts":[{"part_number":1,"etag":" bad"}]}`, httpStatus: 400,
			reads: 1, completions: 1, bodyCalls: 1, consume: true, code: "INVALID_ARGUMENT", details: `{"field":"parts.0.etag"}`},
		{name: "multipart malformed JSON", mode: "MULTIPART", status: domain.UploadStatusUploading, contentType: jsonType,
			body: badBody, httpStatus: 400, reads: 1, completions: 1, bodyCalls: 1, consume: true, code: "INVALID_ARGUMENT"},
		{name: "multipart unsupported media", mode: "MULTIPART", status: domain.UploadStatusUploading, body: validBody,
			contentType: "text/plain", httpStatus: 415, reads: 1, completions: 1, bodyCalls: 1, code: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "multipart oversized body", mode: "MULTIPART", status: domain.UploadStatusUploading, contentType: jsonType,
			body: strings.Repeat(" ", int(binding.MaxJSONBodyBytes)+1), httpStatus: 413, reads: 1, completions: 1, bodyCalls: 1,
			consume: true, code: "REQUEST_BODY_TOO_LARGE"},
		{name: "multipart mismatch", mode: "MULTIPART", status: domain.UploadStatusUploading, contentType: jsonType, body: validBody,
			completionError: &domain.UploadPartsMismatchError{PartNumbers: []uint32{1}}, httpStatus: 409, reads: 1, completions: 1, bodyCalls: 1,
			consume: true, code: "UPLOAD_PARTS_MISMATCH", details: `{"part_numbers":[1]}`},
		{name: "multipart busy before reading", mode: "MULTIPART", status: domain.UploadStatusUploading, body: badBody,
			completionError: domain.ErrUploadTaskBusy, httpStatus: 409, reads: 1, completions: 1, code: "UPLOAD_BUSY"},
		{name: "missing task", lookupError: domain.ErrUploadTaskNotFound, body: badBody, httpStatus: 404, reads: 1, code: "UPLOAD_NOT_FOUND"},
		{name: "foreign task returned", mode: "MULTIPART", wrongOwner: true, body: badBody, httpStatus: 500, reads: 1, code: "INTERNAL_ERROR"},
		{name: "multipart returned single mode", mode: "MULTIPART", status: domain.UploadStatusCompleting, wrongResult: true,
			httpStatus: 500, reads: 1, completions: 1, code: "INTERNAL_ERROR"},
		{name: "canceled task lookup", mode: "MULTIPART", cancelAfterLookup: true, httpStatus: 500, reads: 1, code: "INTERNAL_ERROR"},
		{name: "canceled completion", mode: "MULTIPART", status: domain.UploadStatusCompleting, cancelAfterComplete: true,
			httpStatus: 500, reads: 1, completions: 1, code: "INTERNAL_ERROR"},
		{name: "invalid path", path: "019", body: badBody, httpStatus: 400, code: "INVALID_ARGUMENT", details: `{"field":"id"}`},
		{name: "authentication first", path: "019", body: badBody, noBearer: true, httpStatus: 401, code: "AUTH_REQUIRED"},
	} {
		t.Run(test.name, func(t *testing.T) {
			// 1. 装配临时受保护路由，记录任务读取、模式分流和正文消费。
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			taskID, path := test.taskID, test.path
			if taskID == 0 {
				taskID = 19
			}
			if path == "" {
				path = "19"
			}
			task := uploadTaskSuccessFixture()
			task.ID, task.UploadMode, task.Status = taskID, test.mode, test.status
			if test.status == domain.UploadStatusCompleted {
				task.ReservedBytes = 0
				task.PublishedObjectID, task.ResultEntryID, task.CompletedAt = new(uint64(23)), new(uint64(31)), &task.UpdatedAt
			}
			if test.wrongOwner {
				task.UserID++
			}
			reads, completions, bodyCalls := 0, 0, 0
			assertRequest := func(callContext context.Context, authenticated domain.Actor, id uint64) {
				t.Helper()
				if requestcontext.RequestID(callContext) != requestID || authenticated != actor || id != taskID {
					t.Fatal("处理器未使用可信身份、路径任务 ID 和请求 ID")
				}
			}
			complete := handler.UploadCompletionByMode(
				func(callContext context.Context, authenticated domain.Actor, id uint64) (*domain.UploadTask, error) {
					assertRequest(callContext, authenticated, id)
					reads++
					if test.cancelAfterLookup {
						cancel()
					}
					return task, test.lookupError
				},
				func(callContext context.Context, authenticated domain.Actor, id uint64, readClaims func() ([]domain.UploadPartCompletionClaim, error)) (*domain.UploadTask, error) {
					assertRequest(callContext, authenticated, id)
					completions++
					if test.bodyCalls > 0 {
						bodyCalls++
						received, err := readClaims()
						if err != nil {
							return nil, err
						}
						if !reflect.DeepEqual(received, claims) {
							t.Fatal("回执解析后顺序或值不符", received)
						}
					}
					if test.cancelAfterComplete {
						cancel()
					}
					completed := *task
					if completed.Status == domain.UploadStatusUploading {
						completed.Status = domain.UploadStatusCompleting
					}
					if test.wrongResult {
						completed.UploadMode = "SINGLE"
					}
					return &completed, test.completionError
				})
			protected := middleware.RequireAuthorization(func(_ context.Context, credential string) (*domain.Actor, error) {
				if credential != "fixture-access" {
					return nil, domain.ErrInvalidAccessToken
				}
				return &actor, nil
			}, handler.AuthorizationFailure)
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.POST("/fixture-uploads/:id/complete", protected, complete, func(*gin.Context) { t.Error("响应结束后仍执行下游") })
			bodyReader := strings.NewReader(test.body)
			var requestBody io.Reader
			if test.body != "" {
				requestBody = bodyReader
			}
			request := httptest.NewRequest(http.MethodPost, "/fixture-uploads/"+path+"/complete?user_id=99&id=99", requestBody).WithContext(ctx)
			if test.streamedEmpty {
				request.Body, request.ContentLength = io.NopCloser(bodyReader), -1
			}
			request.Header.Set("X-Request-ID", requestID)
			if !test.noBearer {
				request.Header.Set("Authorization", "Bearer fixture-access")
			}
			if test.contentType != "" {
				request.Header.Set("Content-Type", test.contentType)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			// 2. 确认状态码、正文读取边界和复用的公开响应。
			if response.Code != test.httpStatus || reads != test.reads || completions != test.completions || bodyCalls != test.bodyCalls ||
				(bodyReader.Len() != len(test.body)) != test.consume || response.Header().Get("X-Request-ID") != requestID ||
				response.Header().Get("Cache-Control") != "no-store" || strings.Contains(response.Body.String(), "private-fixture") {
				t.Fatal("完成请求的响应或读取边界不符", response.Code, reads, completions, bodyCalls, response.Body.String())
			}
			if test.code == "" {
				var body dto.UploadTaskResponse
				wantStatus := test.status
				if test.mode == "MULTIPART" && wantStatus == domain.UploadStatusUploading {
					wantStatus = domain.UploadStatusCompleting
				}
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.RequestID != requestID ||
					body.Data.ID != strconv.FormatUint(taskID, 10) || body.Data.UploadMode != test.mode || body.Data.Status != string(wantStatus) {
					t.Fatal("成功响应不符", body, err)
				}
				return
			}
			var body dto.ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.RequestID != requestID || body.Error.Code != test.code {
				t.Fatal("错误响应不符", body, err)
			}
			wantDetails := test.details
			if wantDetails == "" {
				wantDetails = "null"
			}
			details, err := json.Marshal(body.Error.Details)
			if err != nil || string(details) != wantDetails {
				t.Fatalf("错误详情不符，期望 %s，实际 %s，错误 %v", wantDetails, details, err)
			}
		})
	}
}
