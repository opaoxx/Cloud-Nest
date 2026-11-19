package http

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
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

const uploadCancellationSuccessTestPath = "/test-upload-cancellation-success"

func TestUploadCancellationSuccessRealHTTP(t *testing.T) {
	// 1. 为取消中和四种终态准备各自快照，通过真实 HTTP 写出。
	const origin = "http://127.0.0.1:5173"
	const suppliedID = "28EE758A-0727-42DE-AE7D-3BB33155E148"
	scenarios := []struct {
		status     domain.UploadStatus
		httpStatus int
	}{
		{domain.UploadStatusCancelling, http.StatusAccepted},
		{domain.UploadStatusCancelled, http.StatusOK},
		{domain.UploadStatusCompleted, http.StatusOK},
		{domain.UploadStatusExpired, http.StatusOK},
		{domain.UploadStatusFailed, http.StatusOK},
	}
	results := make(map[string]*domain.UploadTask, len(scenarios))
	for _, scenario := range scenarios {
		task := uploadTaskSuccessFixture()
		task.ID, task.Status = math.MaxUint64, scenario.status
		task.LeaseOwner, task.LeaseUntil = nil, nil
		if scenario.status.IsTerminal() {
			task.ReservedBytes = 0
			completedAt := task.UpdatedAt
			task.CompletedAt = &completedAt
		}
		switch scenario.status {
		case domain.UploadStatusCompleted:
			entryID, name := uint64(math.MaxUint64), "发布笔记.MD"
			task.ResultEntryID, task.ResolvedName = &entryID, &name
		case domain.UploadStatusCancelling:
			code := "DATABASE_UNAVAILABLE"
			task.LastErrorCode = &code
		case domain.UploadStatusFailed:
			code := "private-upload-code"
			task.LastErrorCode = &code
		}
		results[string(scenario.status)] = task
	}

	router := NewRouter(logging.New(io.Discard), []string{origin}, func(context.Context) bool { return true })
	router.POST(uploadCancellationSuccessTestPath+"/:state", func(requestContext *gin.Context) {
		requestContext.Header("Content-Length", "3")
		requestContext.Header("Content-Encoding", "gzip")
		requestContext.Header("Content-Type", "application/octet-stream")
		handler.UploadCancellationSuccess(requestContext, results[requestContext.Param("state")])
	}, func(*gin.Context) { t.Error("取消响应写出后继续执行了下游") })
	server := httptest.NewServer(router)
	defer server.Close()
	client := server.Client()
	client.Timeout = 3 * time.Second

	// 2. 对照状态码、请求 ID、公开正文和线上响应头。
	for _, scenario := range scenarios {
		t.Run(string(scenario.status), func(t *testing.T) {
			state := string(scenario.status)
			request, err := http.NewRequest(http.MethodPost, server.URL+uploadCancellationSuccessTestPath+"/"+state, nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Origin", origin)
			if scenario.httpStatus == http.StatusAccepted {
				request.Header.Set("X-Request-ID", suppliedID)
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, readError := io.ReadAll(response.Body)
			closeError := response.Body.Close()
			if readError != nil || closeError != nil || response.StatusCode != scenario.httpStatus ||
				response.ContentLength != int64(len(body)) || response.Header.Get("Content-Encoding") != "" ||
				response.Header.Get("Content-Type") != "application/json; charset=utf-8" ||
				response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Pragma") != "no-cache" ||
				response.Header.Get("Access-Control-Allow-Origin") != origin || len(response.Cookies()) != 0 {
				t.Fatal("真实取消响应状态、正文或头不符", response.StatusCode, readError, closeError, response.Header)
			}
			requestID := response.Header.Get("X-Request-ID")
			parsedID, err := uuid.Parse(requestID)
			if err != nil || parsedID.Version() != 4 ||
				(scenario.httpStatus == http.StatusAccepted && requestID != strings.ToLower(suppliedID)) {
				t.Fatal("未沿用或生成当前请求 ID", requestID, err)
			}
			publicData, err := dto.NewUploadTaskData(results[state])
			if err != nil {
				t.Fatal(err)
			}
			want, err := json.Marshal(dto.UploadTaskResponse{Data: *publicData, RequestID: requestID})
			if err != nil || !bytes.Equal(body, want) || bytes.Contains(body, []byte("private-upload")) {
				t.Fatal("取消响应未保留任务状态、公开字段或请求 ID", string(body), err)
			}
		})
	}
}

func TestUploadCancellationSuccessFailures(t *testing.T) {
	for _, scenario := range []string{"nil task", "multipart", "creating", "uploading", "completing", "verifying", "expiring", "unknown status",
		"invalid task ID", "incomplete publication", "already canceled", "deadline exceeded", "cancel before write"} {
		t.Run(scenario, func(t *testing.T) {
			// 1. 注入异常任务或请求上下文，经真实响应逻辑处理。
			task := uploadTaskSuccessFixture()
			task.Status = domain.UploadStatusCancelling
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.POST(uploadCancellationSuccessTestPath, func(requestContext *gin.Context) {
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
				ctx := requestContext.Request.Context()
				switch scenario {
				case "nil task":
					task = nil
				case "multipart":
					task.UploadMode = "MULTIPART"
				case "creating", "uploading", "completing", "verifying", "expiring", "unknown status":
					task.Status = domain.UploadStatus(strings.ToUpper(scenario))
				case "invalid task ID":
					task.ID = 0
				case "incomplete publication":
					task.Status = domain.UploadStatusCompleted
				case "already canceled":
					canceled, stop := context.WithCancel(ctx)
					stop()
					requestContext.Request = requestContext.Request.WithContext(canceled)
				case "deadline exceeded":
					expired, stop := context.WithDeadline(ctx, time.Unix(0, 0))
					defer stop()
					requestContext.Request = requestContext.Request.WithContext(expired)
				case "cancel before write":
					lateContext, stop := context.WithCancel(ctx)
					defer stop()
					requestContext.Request = requestContext.Request.WithContext(&cancelBeforeRefreshWriteContext{Context: lateContext, cancel: stop})
				}
				handler.UploadCancellationSuccess(requestContext, task)
			}, func(*gin.Context) { t.Error("取消错误响应写出后继续执行了下游") })

			// 2. 核对统一 500 及清理后的正文相关头。
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, uploadCancellationSuccessTestPath, nil))
			var body dto.ErrorResponse
			if response.Code != http.StatusInternalServerError || json.Unmarshal(response.Body.Bytes(), &body) != nil ||
				body.Error.Code != "INTERNAL_ERROR" || body.Error.Details != nil || body.RequestID == "" || body.RequestID != response.Header().Get("X-Request-ID") ||
				strings.Contains(response.Body.String(), `"data"`) || strings.Contains(response.Body.String(), "private-upload") ||
				response.Header().Get("Content-Length") != "" || response.Header().Get("Content-Encoding") != "" ||
				response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" ||
				response.Header().Get("Content-Type") != "application/json; charset=utf-8" {
				t.Fatal("未完整交付取消错误响应", response.Code, response.Body.String(), response.Header())
			}
		})
	}
}

func TestUploadCancellationSuccessEndedResponse(t *testing.T) {
	// 1. 分别准备已写入的响应和主动结束的响应。
	for _, mode := range []string{"written", "aborted"} {
		t.Run(mode, func(t *testing.T) {
			wantBody := ""
			if mode == "written" {
				wantBody = "existing body"
			}
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.POST(uploadCancellationSuccessTestPath, func(requestContext *gin.Context) {
				requestContext.Header("Cache-Control", "private")
				if mode == "written" {
					requestContext.String(http.StatusConflict, wantBody)
				} else {
					requestContext.Status(http.StatusConflict)
					requestContext.Abort()
				}
				handler.UploadCancellationSuccess(requestContext, nil)
			}, func(*gin.Context) { t.Error("已结束的响应继续执行了下游") })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, uploadCancellationSuccessTestPath, nil))
			if response.Code != http.StatusConflict || response.Body.String() != wantBody ||
				response.Header().Get("Cache-Control") != "private" || response.Header().Get("Pragma") != "" {
				t.Fatal("取消交付改写了已结束的响应", response.Code, response.Body.String(), response.Header())
			}
		})
	}
}
