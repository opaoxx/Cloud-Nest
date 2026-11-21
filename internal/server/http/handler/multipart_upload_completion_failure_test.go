package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

func TestMultipartUploadCompletionFailure(t *testing.T) {
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	const privateText = "private-fixture-error"
	mismatch := &domain.UploadPartsMismatchError{PartNumbers: []uint32{2, 9, 10000}}
	limitNumbers := make([]uint32, domain.MaxUploadPartsMismatchNumbers)
	for index := range limitNumbers {
		limitNumbers[index] = uint32(index + 1)
	}
	for _, test := range []struct {
		name, code, message, details string
		err                          error
		status                       int
		canceled                     bool
	}{
		{name: "missing parts", err: &domain.UploadPartDeclarationError{Field: "parts", Message: "完成请求须提交分片回执"},
			status: 400, code: "INVALID_ARGUMENT", message: "完成请求须提交分片回执", details: `{"field":"parts"}`},
		{name: "invalid ETag", err: fmt.Errorf("%s: %w", privateText, &domain.UploadPartDeclarationError{Field: "parts.1.etag", Message: "分片 ETag 格式无效"}),
			status: 400, code: "INVALID_ARGUMENT", message: "分片 ETag 格式无效", details: `{"field":"parts.1.etag"}`},
		{name: "task outside account", err: fmt.Errorf("%s: %w", privateText, domain.ErrUploadTaskNotFound),
			status: 404, code: "UPLOAD_NOT_FOUND", message: "上传任务不存在"},
		{name: "task not ready", err: domain.ErrUploadTaskNotReady, status: 409, code: "TASK_NOT_READY", message: "上传任务当前不可完成"},
		{name: "lease busy", err: domain.ErrUploadTaskBusy, status: 409, code: "UPLOAD_BUSY", message: "上传任务正在处理中"},
		{name: "parts mismatch", err: fmt.Errorf("%s: %w", privateText, mismatch), status: 409, code: "UPLOAD_PARTS_MISMATCH",
			message: "分片回执不一致，请核对后补传", details: `{"part_numbers":[2,9,10000]}`},
		{name: "maximum mismatch list", err: &domain.UploadPartsMismatchError{PartNumbers: limitNumbers},
			status: 409, code: "UPLOAD_PARTS_MISMATCH", details: `{"part_numbers":[1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20]}`},
		{name: "missing mismatch details", err: domain.ErrUploadPartsMismatch, status: 500, code: "INTERNAL_ERROR"},
		{name: "empty mismatch list", err: &domain.UploadPartsMismatchError{}, status: 500, code: "INTERNAL_ERROR"},
		{name: "duplicate mismatch number", err: &domain.UploadPartsMismatchError{PartNumbers: []uint32{3, 3}}, status: 500, code: "INTERNAL_ERROR"},
		{name: "unordered mismatch list", err: &domain.UploadPartsMismatchError{PartNumbers: []uint32{3, 2}}, status: 500, code: "INTERNAL_ERROR"},
		{name: "out of range mismatch number", err: &domain.UploadPartsMismatchError{PartNumbers: []uint32{10001}}, status: 500, code: "INTERNAL_ERROR"},
		{name: "oversized mismatch list", err: &domain.UploadPartsMismatchError{PartNumbers: make([]uint32, domain.MaxUploadPartsMismatchNumbers+1)},
			status: 500, code: "INTERNAL_ERROR"},
		{name: "storage failure", err: fmt.Errorf("%s: %w", privateText, domain.ErrUploadStorageUnavailable), status: 503,
			code: "STORAGE_UNAVAILABLE", message: domain.ErrUploadStorageUnavailable.Error()},
		{name: "storage session unavailable", err: domain.ErrUploadPartSessionMissing, status: 503, code: "STORAGE_UNAVAILABLE"},
		{name: "session revoked", err: domain.ErrSessionRevoked, status: 401, code: "SESSION_REVOKED"},
		{name: "account disabled", err: domain.ErrAccountDisabled, status: 403, code: "ACCOUNT_DISABLED"},
		{name: "invalid internal task", err: domain.ErrInvalidUploadTaskData, status: 500, code: "INTERNAL_ERROR"},
		{name: "release error does not replace mismatch", err: &domain.UploadCompletionReleaseError{Cause: mismatch, ReleaseError: domain.ErrSessionRevoked},
			status: 409, code: "UPLOAD_PARTS_MISMATCH", details: `{"part_numbers":[2,9,10000]}`},
		{name: "cleanup authorization error does not classify primary failure",
			err:    fmt.Errorf("outer: %w", &domain.UploadCompletionReleaseError{Cause: errors.New(privateText), ReleaseError: domain.ErrSessionRevoked}),
			status: 500, code: "INTERNAL_ERROR"},
		{name: "request canceled", err: mismatch, canceled: true, status: 500, code: "INTERNAL_ERROR"},
	} {
		t.Run(test.name, func(t *testing.T) {
			// 1. 用内存 HTTP 请求交付错误，预置可能来自上游的正文头。
			ctx := requestcontext.WithRequestID(t.Context(), requestID)
			if test.canceled {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			}
			router := gin.New()
			router.POST("/fixture", func(requestContext *gin.Context) {
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
				requestContext.Header("Content-Type", "application/octet-stream")
				writeMultipartUploadCompletionFailure(requestContext, test.err)
			}, func(*gin.Context) { t.Error("错误响应写出后仍进入下游") })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/fixture", nil).WithContext(ctx))

			// 2. 核对固定的公开 JSON、请求 ID 和响应头。
			var body dto.ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != test.status ||
				body.RequestID != requestID || body.Error.Code != test.code || body.Error.Message == "" ||
				(test.message != "" && body.Error.Message != test.message) || strings.Contains(response.Body.String(), privateText) {
				t.Fatal("错误响应契约不符", response.Code, response.Body.String(), err)
			}
			wantDetails := test.details
			if wantDetails == "" {
				wantDetails = "null"
			}
			details, err := json.Marshal(body.Error.Details)
			if err != nil || string(details) != wantDetails {
				t.Fatal("错误详情不符", string(details), err)
			}
			header := response.Header()
			if header.Get("Cache-Control") != "no-store" || header.Get("Pragma") != "no-cache" ||
				header.Get("Content-Type") != "application/json; charset=utf-8" ||
				header.Get("Content-Length") != "" || header.Get("Content-Encoding") != "" ||
				(header.Get("WWW-Authenticate") == `Bearer realm="cloudnest"`) != (test.status == 401) {
				t.Fatal("错误响应头不符", header)
			}
		})
	}
}

func TestMultipartUploadCompletionFailureEndedResponse(t *testing.T) {
	for _, written := range []bool{false, true} {
		response := httptest.NewRecorder()
		requestContext, _ := gin.CreateTestContext(response)
		requestContext.Header("Cache-Control", "private")
		if written {
			requestContext.String(http.StatusConflict, "existing body")
		} else {
			requestContext.Abort()
		}
		// 1. 已完成或中止的响应保留原正文和响应头。
		writeMultipartUploadCompletionFailure(requestContext, domain.ErrUploadTaskBusy)
		expectedBody := ""
		if written {
			expectedBody = "existing body"
		}
		if response.Body.String() != expectedBody || response.Header().Get("Cache-Control") != "private" ||
			response.Header().Get("Pragma") != "" || !requestContext.IsAborted() {
			t.Fatal("错误映射改写了已结束的响应", response.Body.String(), response.Header())
		}
	}
}
