package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
)

func TestUploadContentSuccessRealHTTP(t *testing.T) {
	const path = "/test-upload-content-success"
	const origin = "http://127.0.0.1:5173"
	const suppliedID = "28EE758A-0727-42DE-AE7D-3BB33155E148"
	task := uploadTaskSuccessFixture()
	task.ID, task.Status = ^uint64(0), domain.UploadStatusVerifying
	router := NewRouter(logging.New(io.Discard), []string{origin}, func(context.Context) bool { return true })
	router.PUT(path, func(requestContext *gin.Context) {
		requestContext.Header("Content-Length", "3")
		requestContext.Header("Content-Encoding", "gzip")
		requestContext.Header("Content-Type", "application/octet-stream")
		handler.UploadContentSuccess(requestContext, task)
	}, func(*gin.Context) { t.Error("成功交付后继续执行了下游") })
	server := httptest.NewServer(router)
	defer server.Close()
	client := server.Client()
	client.Timeout = 3 * time.Second
	var previousID string
	for _, requestedID := range []string{suppliedID, ""} {
		request, err := http.NewRequest(http.MethodPut, server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Origin", origin)
		if requestedID != "" {
			request.Header.Set("X-Request-ID", requestedID)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, readError := io.ReadAll(response.Body)
		closeError := response.Body.Close()
		if readError != nil || closeError != nil || response.StatusCode != http.StatusAccepted ||
			response.ContentLength != int64(len(body)) || response.Header.Get("Content-Encoding") != "" ||
			response.Header.Get("Content-Type") != "application/json; charset=utf-8" ||
			response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Pragma") != "no-cache" ||
			response.Header.Get("Access-Control-Allow-Origin") != origin || len(response.Cookies()) != 0 {
			t.Fatal("真实 202 响应状态、正文或头不符", response.StatusCode, readError, closeError)
		}
		responseID := response.Header.Get("X-Request-ID")
		parsedID, err := uuid.Parse(responseID)
		if err != nil || parsedID.Version() != 4 || responseID == previousID ||
			(requestedID != "" && responseID != strings.ToLower(requestedID)) {
			t.Fatal("没有沿用或生成当前请求 ID", responseID, err)
		}
		want := `{"data":{"task_id":"18446744073709551615","status":"VERIFYING"},"request_id":"` + responseID + `"}`
		if string(body) != want {
			t.Fatal("202 正文包含错误字段、状态或 ID", string(body))
		}
		previousID = responseID
	}
}

func TestUploadContentSuccessFailures(t *testing.T) {
	for _, mode := range []string{"no task", "zero task ID", "still uploading", "already canceled", "deadline exceeded", "cancel before write"} {
		t.Run(mode, func(t *testing.T) {
			task := uploadTaskSuccessFixture()
			task.Status = domain.UploadStatusVerifying
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.PUT("/test-upload-content-success", func(requestContext *gin.Context) {
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
				ctx := requestContext.Request.Context()
				switch mode {
				case "no task":
					task = nil
				case "zero task ID":
					task.ID = 0
				case "still uploading":
					task.Status = domain.UploadStatusUploading
				case "already canceled":
					canceled, stop := context.WithCancel(ctx)
					stop()
					requestContext.Request = requestContext.Request.WithContext(canceled)
				case "deadline exceeded":
					expired, stop := context.WithDeadline(ctx, time.Now().Add(-time.Second))
					defer stop()
					requestContext.Request = requestContext.Request.WithContext(expired)
				case "cancel before write":
					lateContext, stop := context.WithCancel(ctx)
					defer stop()
					requestContext.Request = requestContext.Request.WithContext(&cancelBeforeRefreshWriteContext{Context: lateContext, cancel: stop})
				}
				handler.UploadContentSuccess(requestContext, task)
			}, func(*gin.Context) { t.Error("错误交付后继续执行了下游") })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/test-upload-content-success", nil))
			var body dto.ErrorResponse
			if response.Code != http.StatusInternalServerError || json.Unmarshal(response.Body.Bytes(), &body) != nil ||
				body.Error.Code != "INTERNAL_ERROR" || body.Error.Details != nil || body.RequestID == "" ||
				body.RequestID != response.Header().Get("X-Request-ID") || strings.Contains(response.Body.String(), `"data"`) ||
				strings.Contains(response.Body.String(), "private-upload") || response.Header().Get("Content-Length") != "" ||
				response.Header().Get("Content-Encoding") != "" || response.Header().Get("Cache-Control") != "no-store" ||
				response.Header().Get("Pragma") != "no-cache" || response.Header().Get("Content-Type") != "application/json; charset=utf-8" {
				t.Fatal("未完整交付统一错误响应", response.Code, response.Body.String(), response.Header())
			}
		})
	}
}

func TestUploadContentSuccessEndedResponse(t *testing.T) {
	for _, mode := range []string{"written", "aborted"} {
		t.Run(mode, func(t *testing.T) {
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.PUT("/test-upload-content-success", func(requestContext *gin.Context) {
				requestContext.Header("Cache-Control", "private")
				if mode == "written" {
					requestContext.String(http.StatusConflict, "existing body")
				} else {
					requestContext.Status(http.StatusConflict)
					requestContext.Abort()
				}
				handler.UploadContentSuccess(requestContext, nil)
			}, func(*gin.Context) { t.Error("原处理已结束却进入下游") })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/test-upload-content-success", nil))
			wantBody := ""
			if mode == "written" {
				wantBody = "existing body"
			}
			if response.Code != http.StatusConflict || response.Body.String() != wantBody ||
				response.Header().Get("Cache-Control") != "private" || response.Header().Get("Pragma") != "" {
				t.Fatal("响应处理已结束却被改写", response.Code, response.Body.String())
			}
		})
	}
}
