package http

import (
	"bytes"
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
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
)

const trashSuccessTestPath = "/test-trash-entry-success"

const trashSuccessTestBody = `{
  "entry": {
    "deleted_at": "2026-11-20T04:30:00.123456Z",
    "name": "private-snapshot 学习笔记.md",
    "status": "TRASHED",
    "revision": "18446744073709551615",
    "id": "18446744073709551615",
    "parent_id": "18446744073709551614",
    "entry_type": "FILE"
  }
}`

func TestTrashEntrySuccessHTTP(t *testing.T) {
	const origin = "http://127.0.0.1:5173"
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	for _, testCase := range []struct {
		name, entryType, suppliedID string
	}{
		{"file with supplied request ID", "FILE", strings.ToUpper(requestID)},
		{"directory with generated request ID", "DIR", ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 通过真实 TCP 测试入口发送已保存快照，模拟未提交的旧响应头。
			stableBody := []byte(strings.Replace(trashSuccessTestBody, `"FILE"`, `"`+testCase.entryType+`"`, 1))
			original := bytes.Clone(stableBody)
			expected, err := dto.RestoreTrashEntryData(stableBody)
			if err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			router := NewRouter(logging.New(&logs), []string{origin}, func(context.Context) bool { return true })
			router.POST(trashSuccessTestPath, func(requestContext *gin.Context) {
				requestContext.Header("Content-Type", "application/octet-stream")
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
				handler.TrashEntrySuccess(requestContext, stableBody)
			}, func(*gin.Context) { t.Error("交付回收快照后仍执行了下游") })
			server := httptest.NewServer(router)
			defer server.Close()
			client := server.Client()
			client.Timeout = 3 * time.Second
			request, err := http.NewRequest(http.MethodPost, server.URL+trashSuccessTestPath+"?note=private-query", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Origin", origin)
			request.Header.Set("Accept-Encoding", "identity")
			request.Header.Set("Idempotency-Key", "private-replay-key")
			if testCase.suppliedID != "" {
				request.Header.Set("X-Request-ID", testCase.suppliedID)
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, readError := io.ReadAll(response.Body)
			closeError := response.Body.Close()
			server.Close()

			// 2. 核对 200、正文头、七字段快照与请求 ID。
			if readError != nil || closeError != nil || response.StatusCode != http.StatusOK ||
				response.ContentLength != int64(len(body)) || response.Header.Get("Content-Encoding") != "" ||
				response.Header.Get("Content-Type") != "application/json; charset=utf-8" ||
				response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Pragma") != "no-cache" ||
				response.Header.Get("Access-Control-Allow-Origin") != origin {
				t.Fatal("回收成功响应状态或头不符", response.StatusCode, readError, closeError)
			}
			actualID := response.Header.Get("X-Request-ID")
			parsedID, err := uuid.Parse(actualID)
			if err != nil || parsedID.Version() != 4 || (testCase.suppliedID != "" && actualID != requestID) {
				t.Fatal("回收响应请求 ID 不符", actualID, err)
			}
			expectedBody, err := json.Marshal(dto.TrashEntryResponse{Data: *expected, RequestID: actualID})
			if err != nil || !bytes.Equal(body, expectedBody) || !bytes.Equal(stableBody, original) {
				t.Fatal("公开正文不符或修改了稳定结果", err)
			}
			var recorded map[string]any
			if err := json.Unmarshal(logs.Bytes(), &recorded); err != nil || recorded["request_id"] != actualID ||
				recorded["status"] != float64(http.StatusOK) || strings.Contains(logs.String(), "private-") {
				t.Fatal("完成日志关联不符或包含快照正文", err)
			}
		})
	}
}

func TestTrashEntrySuccessFailures(t *testing.T) {
	for _, scenario := range []string{"nil snapshot", "missing field", "active entry", "canceled", "deadline", "cancel before write"} {
		t.Run(scenario, func(t *testing.T) {
			// 1. 直接向交付方法提供异常快照或已取消的请求。
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.POST(trashSuccessTestPath, func(requestContext *gin.Context) {
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
				stableBody := []byte(trashSuccessTestBody)
				ctx := requestContext.Request.Context()
				switch scenario {
				case "nil snapshot":
					stableBody = nil
				case "missing field":
					stableBody = bytes.Replace(stableBody, []byte(`"status": "TRASHED",`), nil, 1)
				case "active entry":
					stableBody = bytes.Replace(stableBody, []byte(`"TRASHED"`), []byte(`"ACTIVE"`), 1)
				case "canceled":
					canceled, stop := context.WithCancel(ctx)
					stop()
					requestContext.Request = requestContext.Request.WithContext(canceled)
				case "deadline":
					expired, stop := context.WithDeadline(ctx, time.Unix(0, 0))
					defer stop()
					requestContext.Request = requestContext.Request.WithContext(expired)
				case "cancel before write":
					cancelable, stop := context.WithCancel(ctx)
					defer stop()
					requestContext.Request = requestContext.Request.WithContext(&cancelBeforeRefreshWriteContext{Context: cancelable, cancel: stop})
				}
				handler.TrashEntrySuccess(requestContext, stableBody)
			}, func(*gin.Context) { t.Error("失败响应发出后执行了下游") })

			// 2. 核对统一 500 和当前请求 ID，清除旧正文头。
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, trashSuccessTestPath, nil))
			var failure dto.ErrorResponse
			header := response.Header()
			if response.Code != http.StatusInternalServerError || json.Unmarshal(response.Body.Bytes(), &failure) != nil ||
				failure.Error.Code != "INTERNAL_ERROR" || failure.Error.Details != nil ||
				failure.RequestID == "" || failure.RequestID != header.Get("X-Request-ID") ||
				header.Get("Cache-Control") != "no-store" || header.Get("Pragma") != "no-cache" ||
				header.Get("Content-Type") != "application/json; charset=utf-8" ||
				header.Get("Content-Length") != "" || header.Get("Content-Encoding") != "" ||
				strings.Contains(response.Body.String(), "private-snapshot") || strings.Contains(response.Body.String(), `"data"`) {
				t.Fatal("异常回收快照未按统一错误交付", response.Code)
			}
		})
	}
}

func TestTrashEntrySuccessEndedResponse(t *testing.T) {
	for _, mode := range []string{"written", "aborted"} {
		t.Run(mode, func(t *testing.T) {
			wantBody := ""
			if mode == "written" {
				wantBody = "existing response"
			}
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.POST(trashSuccessTestPath, func(requestContext *gin.Context) {
				// 1. 结束响应后调用交付入口，已确定的响应保持原样。
				requestContext.Header("Cache-Control", "private")
				if mode == "written" {
					requestContext.String(http.StatusConflict, wantBody)
				} else {
					requestContext.Status(http.StatusConflict)
					requestContext.Abort()
				}
				handler.TrashEntrySuccess(requestContext, []byte(trashSuccessTestBody))
			}, func(*gin.Context) { t.Error("已经结束的响应继续执行了下游") })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, trashSuccessTestPath, nil))
			if response.Code != http.StatusConflict || response.Body.String() != wantBody ||
				response.Header().Get("Cache-Control") != "private" || response.Header().Get("Pragma") != "" {
				t.Fatal("回收交付改写了已结束响应", response.Code)
			}
		})
	}
}
