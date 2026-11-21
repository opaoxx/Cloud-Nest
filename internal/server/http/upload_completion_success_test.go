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

const uploadCompletionSuccessTestPath = "/test-upload-completion-success"

func TestUploadCompletionSuccessRealHTTP(t *testing.T) {
	const origin = "http://127.0.0.1:5173"
	const suppliedID = "28EE758A-0727-42DE-AE7D-3BB33155E148"
	for _, scenario := range []struct {
		mode   string
		status domain.UploadStatus
		code   int
	}{
		{"SINGLE", domain.UploadStatusVerifying, http.StatusAccepted},
		{"SINGLE", domain.UploadStatusCompleted, http.StatusOK},
		{"MULTIPART", domain.UploadStatusCompleting, http.StatusAccepted},
		{"MULTIPART", domain.UploadStatusVerifying, http.StatusAccepted},
		{"MULTIPART", domain.UploadStatusCompleted, http.StatusOK},
	} {
		t.Run(scenario.mode+"/"+string(scenario.status), func(t *testing.T) {
			// 1. 通过真实 POST 交付一次已确认的任务快照。
			task := uploadTaskSuccessFixture()
			task.ID, task.Status, task.UploadMode = math.MaxUint64, scenario.status, scenario.mode
			task.LeaseOwner, task.LeaseUntil = nil, nil
			if scenario.mode == "MULTIPART" {
				task.FileSize, task.ReservedBytes = (16<<20)+1, (16<<20)+1
				task.PartSize, task.PartCount = new(uint64(16<<20)), new(uint32(2))
				task.StorageUploadID = new("private-upload-session")
			}
			if scenario.status == domain.UploadStatusCompleted {
				objectID, entryID, name := uint64(23), uint64(math.MaxUint64), "发布笔记.MD"
				task.ReservedBytes = 0
				task.PublishedObjectID, task.ResultEntryID, task.CompletedAt = &objectID, &entryID, &task.UpdatedAt
				task.ResolvedName = &name
			} else {
				code, retryAt := "DATABASE_UNAVAILABLE", task.UpdatedAt.Add(16*time.Second)
				task.LastErrorCode, task.NextAttemptAt = &code, &retryAt
			}
			publicData, err := dto.NewUploadTaskData(task)
			if err != nil {
				t.Fatal(err)
			}
			router := NewRouter(logging.New(io.Discard), []string{origin}, func(context.Context) bool { return true })
			router.POST(uploadCompletionSuccessTestPath, func(requestContext *gin.Context) {
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
				requestContext.Header("Content-Type", "application/octet-stream")
				handler.UploadCompletionSuccess(requestContext, task)
			}, func(*gin.Context) { t.Error("完成响应写出后继续执行了下游") })
			server := httptest.NewServer(router)
			defer server.Close()
			client := server.Client()
			client.Timeout = 3 * time.Second
			request, err := http.NewRequest(http.MethodPost, server.URL+uploadCompletionSuccessTestPath, nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Origin", origin)
			if scenario.status == domain.UploadStatusVerifying {
				request.Header.Set("X-Request-ID", suppliedID)
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, readError := io.ReadAll(response.Body)
			closeError := response.Body.Close()

			// 2. 核对 HTTP 状态、查询接口的完整公开字段和禁止缓存元数据。
			if readError != nil || closeError != nil || response.StatusCode != scenario.code || response.ContentLength != int64(len(body)) ||
				response.Header.Get("Content-Encoding") != "" || response.Header.Get("Content-Type") != "application/json; charset=utf-8" ||
				response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Pragma") != "no-cache" ||
				response.Header.Get("Access-Control-Allow-Origin") != origin || len(response.Cookies()) != 0 {
				t.Fatal("完成确认的真实 HTTP 状态、正文或响应头不符", response.StatusCode, readError, closeError, response.Header)
			}
			requestID := response.Header.Get("X-Request-ID")
			parsedID, err := uuid.Parse(requestID)
			if err != nil || parsedID.Version() != 4 ||
				(scenario.status == domain.UploadStatusVerifying && requestID != strings.ToLower(suppliedID)) {
				t.Fatal("完成响应未沿用或生成当前请求 ID", requestID, err)
			}
			want, err := json.Marshal(dto.UploadTaskResponse{Data: *publicData, RequestID: requestID})
			if err != nil || !bytes.Equal(body, want) || bytes.Contains(body, []byte("private-upload")) {
				t.Fatal("完成响应未保留公开任务快照或泄露内部信息", string(body), err)
			}
			var decoded dto.UploadTaskResponse
			if err := json.Unmarshal(body, &decoded); err != nil ||
				decoded.Data.UploadMode != scenario.mode || decoded.Data.Status != string(scenario.status) {
				t.Fatal("响应未保留原上传模式和当前阶段", err)
			}
			if scenario.code == http.StatusOK {
				if decoded.Data.ResultEntryID == nil || *decoded.Data.ResultEntryID != "18446744073709551615" ||
					decoded.Data.CompletedAt == nil || !decoded.Data.CompletedAt.Equal(task.UpdatedAt.UTC().Truncate(time.Microsecond)) {
					t.Fatal("200 未交付原发布条目 ID 和 UTC 微秒完成时间")
				}
			} else if decoded.Data.ResultEntryID != nil || decoded.Data.CompletedAt != nil {
				t.Fatal("202 包含尚未发布的结果")
			}
		})
	}
}

func TestUploadCompletionSuccessFailures(t *testing.T) {
	for _, scenario := range []string{"nil result", "single completing", "multipart uploading", "multipart completing with result",
		"multipart completed with reservation", "uploading", "canceled task", "invalid published result",
		"verifying with result entry", "verifying with completion time", "invalid public ID", "request canceled", "deadline exceeded", "cancel before response"} {
		t.Run(scenario, func(t *testing.T) {
			// 1. 注入相互矛盾的任务字段或到期的请求上下文。
			task := uploadTaskSuccessFixture()
			task.Status = domain.UploadStatusVerifying
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.POST(uploadCompletionSuccessTestPath, func(requestContext *gin.Context) {
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
				ctx := requestContext.Request.Context()
				switch scenario {
				case "nil result":
					task = nil
				case "single completing":
					task.Status = domain.UploadStatusCompleting
				case "multipart uploading":
					task.UploadMode, task.Status = "MULTIPART", domain.UploadStatusUploading
				case "multipart completing with result":
					task.UploadMode, task.Status, task.ResultEntryID = "MULTIPART", domain.UploadStatusCompleting, new(uint64(31))
				case "multipart completed with reservation":
					task.UploadMode, task.Status = "MULTIPART", domain.UploadStatusCompleted
					task.PublishedObjectID, task.ResultEntryID, task.CompletedAt = new(uint64(23)), new(uint64(31)), &task.UpdatedAt
				case "uploading":
					task.Status = domain.UploadStatusUploading
				case "canceled task":
					task.Status, task.ReservedBytes = domain.UploadStatusCancelled, 0
				case "invalid published result":
					task.Status, task.ReservedBytes = domain.UploadStatusCompleted, 0
					task.CompletedAt = &task.UpdatedAt
				case "verifying with result entry":
					entryID := uint64(31)
					task.ResultEntryID = &entryID
				case "verifying with completion time":
					task.CompletedAt = &task.UpdatedAt
				case "invalid public ID":
					task.ID = 0
				case "request canceled":
					canceled, stop := context.WithCancel(ctx)
					stop()
					requestContext.Request = requestContext.Request.WithContext(canceled)
				case "deadline exceeded":
					expired, stop := context.WithDeadline(ctx, time.Unix(0, 0))
					defer stop()
					requestContext.Request = requestContext.Request.WithContext(expired)
				case "cancel before response":
					lateContext, stop := context.WithCancel(ctx)
					defer stop()
					requestContext.Request = requestContext.Request.WithContext(&cancelBeforeRefreshWriteContext{Context: lateContext, cancel: stop})
				}
				handler.UploadCompletionSuccess(requestContext, task)
			}, func(*gin.Context) { t.Error("完成错误响应写出后继续执行了下游") })

			// 2. 异常的成功交付使用统一 500，清理暂存正文头。
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, uploadCompletionSuccessTestPath, nil))
			var body dto.ErrorResponse
			if response.Code != http.StatusInternalServerError || json.Unmarshal(response.Body.Bytes(), &body) != nil ||
				body.Error.Code != "INTERNAL_ERROR" || body.Error.Details != nil || body.RequestID == "" || body.RequestID != response.Header().Get("X-Request-ID") ||
				strings.Contains(response.Body.String(), `"data"`) || strings.Contains(response.Body.String(), "private-upload") ||
				response.Header().Get("Content-Length") != "" || response.Header().Get("Content-Encoding") != "" ||
				response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" ||
				response.Header().Get("Content-Type") != "application/json; charset=utf-8" {
				t.Fatal("完成确认的错误交付不符", response.Code, response.Body.String(), response.Header())
			}
		})
	}
}

func TestUploadCompletionSuccessEndedResponse(t *testing.T) {
	for _, mode := range []string{"written", "aborted"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 先由其他处理逻辑结束响应，核对完成交付保留原结果。
			wantBody := ""
			if mode == "written" {
				wantBody = "existing body"
			}
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.POST(uploadCompletionSuccessTestPath, func(requestContext *gin.Context) {
				requestContext.Header("Cache-Control", "private")
				if mode == "written" {
					requestContext.String(http.StatusConflict, wantBody)
				} else {
					requestContext.Status(http.StatusConflict)
					requestContext.Abort()
				}
				handler.UploadCompletionSuccess(requestContext, nil)
			}, func(*gin.Context) { t.Error("已结束的响应继续执行了下游") })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, uploadCompletionSuccessTestPath, nil))
			if response.Code != http.StatusConflict || response.Body.String() != wantBody ||
				response.Header().Get("Cache-Control") != "private" || response.Header().Get("Pragma") != "" {
				t.Fatal("完成确认改写了已结束的响应", response.Code, response.Body.String(), response.Header())
			}
		})
	}
}
