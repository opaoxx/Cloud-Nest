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

const restoreSuccessTestPath = "/test-restore-entry-success"

const restoreSuccessTestBody = `{
  "entry": {
    "name": "private-snapshot 学习笔记 (1).md",
    "status": "ACTIVE",
    "revision": "18446744073709551615",
    "id": "18446744073709551613",
    "parent_id": "18446744073709551614",
    "entry_type": "FILE"
  }
}`

func TestRestoreEntrySuccessHTTP(t *testing.T) {
	const origin = "http://127.0.0.1:5173"
	const suppliedID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	stableBody := []byte(restoreSuccessTestBody)
	original := bytes.Clone(stableBody)
	var logs bytes.Buffer
	router := NewRouter(logging.New(&logs), []string{origin}, func(context.Context) bool { return true })
	router.POST(restoreSuccessTestPath, func(requestContext *gin.Context) {
		requestContext.Header("Content-Type", "application/octet-stream")
		requestContext.Header("Content-Length", "3")
		requestContext.Header("Content-Encoding", "gzip")
		handler.RestoreEntrySuccess(requestContext, stableBody)
	}, func(*gin.Context) { t.Error("交付成功响应后继续执行了下游") })
	server := httptest.NewServer(router)
	defer server.Close()
	client := server.Client()
	client.Timeout = 3 * time.Second

	// 1. 对同一快照发出两次请求，分别带上显式 ID 和由中间件生成的 ID。
	var firstID string
	for _, rawID := range []string{strings.ToUpper(suppliedID), ""} {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+restoreSuccessTestPath+"?note=private-query", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Origin", origin)
		request.Header.Set("Accept-Encoding", "identity")
		request.Header.Set("Idempotency-Key", "private-replay-key")
		if rawID != "" {
			request.Header.Set("X-Request-ID", rawID)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, readError := io.ReadAll(response.Body)
		closeError := response.Body.Close()
		actualID := response.Header.Get("X-Request-ID")
		parsedID, idError := uuid.Parse(actualID)
		if idError != nil || parsedID.Version() != 4 || parsedID.Variant() != uuid.RFC4122 ||
			(rawID != "" && actualID != suppliedID) || actualID == firstID {
			t.Fatal("请求 ID 未按当前请求生成或规范化", actualID, idError)
		}
		if firstID == "" {
			firstID = actualID
		}

		// 2. 核对正文、当次请求 ID、缓存策略及实际传输长度。
		want := dto.RestoreEntryResponse{
			Data: dto.RestoreEntryData{Entry: dto.RestoredEntrySummary{
				ID: "18446744073709551613", ParentID: "18446744073709551614", Name: "private-snapshot 学习笔记 (1).md",
				EntryType: "FILE", Status: "ACTIVE", Revision: "18446744073709551615",
			}},
			RequestID: actualID,
		}
		expectedBody, err := json.Marshal(want)
		if readError != nil || closeError != nil || err != nil || response.StatusCode != http.StatusOK ||
			response.ContentLength != int64(len(body)) || response.Header.Get("Content-Encoding") != "" ||
			response.Header.Get("Content-Type") != "application/json; charset=utf-8" ||
			response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Pragma") != "no-cache" ||
			response.Header.Get("Access-Control-Allow-Origin") != origin || !bytes.Equal(body, expectedBody) {
			t.Fatal("恢复成功响应正文或头不符", response.StatusCode, readError, closeError, err)
		}
	}
	server.Close()
	if !bytes.Equal(stableBody, original) || strings.Contains(logs.String(), "private-") {
		t.Fatal("输入快照被修改，或私有数据进入完成日志")
	}
}

func TestRestoreEntrySuccessFailures(t *testing.T) {
	for _, scenario := range []string{"nil snapshot", "missing parent", "trashed status", "duplicate field", "canceled", "deadline", "cancel before write"} {
		t.Run(scenario, func(t *testing.T) {
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.POST(restoreSuccessTestPath, func(requestContext *gin.Context) {
				stableBody := []byte(restoreSuccessTestBody)
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
				ctx := requestContext.Request.Context()
				switch scenario {
				case "nil snapshot":
					stableBody = nil
				case "missing parent":
					stableBody = bytes.Replace(stableBody, []byte(`"parent_id": "18446744073709551614",`), nil, 1)
				case "trashed status":
					stableBody = bytes.Replace(stableBody, []byte(`"ACTIVE"`), []byte(`"TRASHED"`), 1)
				case "duplicate field":
					stableBody = bytes.Replace(stableBody, []byte(`"status":`), []byte(`"status":"ACTIVE","status":`), 1)
				case "canceled":
					cancelable, stop := context.WithCancel(ctx)
					stop()
					requestContext.Request = requestContext.Request.WithContext(cancelable)
				case "deadline":
					expired, stop := context.WithDeadline(ctx, time.Unix(0, 0))
					defer stop()
					requestContext.Request = requestContext.Request.WithContext(expired)
				case "cancel before write":
					cancelable, stop := context.WithCancel(ctx)
					defer stop()
					requestContext.Request = requestContext.Request.WithContext(&cancelBeforeRefreshWriteContext{Context: cancelable, cancel: stop})
				}
				handler.RestoreEntrySuccess(requestContext, stableBody)
			}, func(*gin.Context) { t.Error("交付错误响应后继续执行了下游") })

			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, restoreSuccessTestPath, nil))
			var failure dto.ErrorResponse
			header := response.Header()
			if response.Code != http.StatusInternalServerError || json.Unmarshal(response.Body.Bytes(), &failure) != nil ||
				failure.Error.Code != "INTERNAL_ERROR" || failure.Error.Details != nil || failure.RequestID == "" ||
				failure.RequestID != header.Get("X-Request-ID") || header.Get("Cache-Control") != "no-store" ||
				header.Get("Pragma") != "no-cache" || header.Get("Content-Type") != "application/json; charset=utf-8" ||
				header.Get("Content-Length") != "" || header.Get("Content-Encoding") != "" ||
				strings.Contains(response.Body.String(), "private-snapshot") || strings.Contains(response.Body.String(), `"data"`) {
				t.Fatal("异常交付没有返回统一 500", response.Code)
			}
		})
	}
}

func TestRestoreEntrySuccessEndedResponse(t *testing.T) {
	for _, mode := range []string{"written", "aborted"} {
		t.Run(mode, func(t *testing.T) {
			wantBody := ""
			if mode == "written" {
				wantBody = "existing response"
			}
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.POST(restoreSuccessTestPath, func(requestContext *gin.Context) {
				requestContext.Header("Cache-Control", "private")
				if mode == "written" {
					requestContext.String(http.StatusConflict, wantBody)
				} else {
					requestContext.Status(http.StatusConflict)
					requestContext.Abort()
				}
				handler.RestoreEntrySuccess(requestContext, []byte(restoreSuccessTestBody))
			}, func(*gin.Context) { t.Error("已结束的响应继续执行了下游") })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, restoreSuccessTestPath, nil))
			if response.Code != http.StatusConflict || response.Body.String() != wantBody ||
				response.Header().Get("Cache-Control") != "private" || response.Header().Get("Pragma") != "" {
				t.Fatal("已结束的响应被改写", response.Code)
			}
		})
	}
}
