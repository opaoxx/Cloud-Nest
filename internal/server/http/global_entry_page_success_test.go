package http

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

func globalEntryPageSuccessFixture() *domain.GlobalEntryPageResult {
	result := directoryPageSuccessFixture()
	return &domain.GlobalEntryPageResult{Entries: result.Entries, NextCursor: result.NextCursor}
}

func TestGlobalEntryPageSuccessDelivery(t *testing.T) {
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	for _, testCase := range []struct {
		name            string
		requestIDs      []string
		expectedID      string
		origin, pending bool
	}{
		{name: "existing request id", requestIDs: []string{requestID}, expectedID: requestID},
		{name: "uppercase request id", requestIDs: []string{strings.ToUpper(requestID)}, expectedID: requestID},
		{name: "generated request id"},
		{name: "invalid request id", requestIDs: []string{"private-request-id"}},
		{name: "duplicate request id", requestIDs: []string{requestID, requestID}},
		{name: "allowed origin", origin: true},
		{name: "pending body headers", pending: true},
		{name: "empty page"},
		{name: "last page"},
		{name: "mixed parents"},
		{name: "budget empty page"},
		{name: "maximum cursor"},
		{name: "maximum ids"},
		{name: "safe byte boundary"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 测试专用 GET 入口交付成功全局页，复用公共中间件。
			result := globalEntryPageSuccessFixture()
			switch testCase.name {
			case "empty page":
				result.Entries, result.NextCursor = nil, nil
			case "last page":
				result.NextCursor = nil
			case "mixed parents":
				entry := result.Entries[0]
				parentID := uint64(14)
				entry.ID, entry.ParentID = 24, &parentID
				result.Entries = append(result.Entries, entry)
			case "budget empty page":
				result.Entries = nil
			case "maximum cursor":
				cursor := strings.Repeat("a", domain.MaxGlobalEntryCursorLength)
				result.NextCursor = &cursor
			case "maximum ids":
				maximum := ^uint64(0)
				parentID := maximum - 1
				result.Entries[0].ID, result.Entries[0].UserID, result.Entries[0].ParentID, result.Entries[0].Revision = maximum, maximum, &parentID, maximum
			case "safe byte boundary":
				result.Entries[0].FileSize = 1<<53 - 1
			}
			original, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			expected, err := dto.NewGlobalEntryPageData(result)
			if err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			var storedID string
			origin := "http://127.0.0.1:5173"
			router := NewRouter(logging.New(&logs), []string{origin}, func(context.Context) bool { return true })
			router.GET("/test-global-entry-page-success", func(requestContext *gin.Context) {
				storedID = requestcontext.RequestID(requestContext.Request.Context())
				if testCase.pending {
					requestContext.Header("Content-Length", "3")
					requestContext.Header("Content-Encoding", "gzip")
					requestContext.Header("Content-Type", "application/octet-stream")
				}
				handler.GlobalEntryPageSuccess(requestContext, result)
			}, func(*gin.Context) { t.Error("成功交付后进入下游") })
			request := httptest.NewRequest("GET", "/test-global-entry-page-success?note=private-query", nil)
			for _, value := range testCase.requestIDs {
				request.Header.Add("X-Request-ID", value)
			}
			if testCase.origin {
				request.Header.Set("Origin", origin)
			}
			request.Header.Set("Authorization", "Bearer private-access")
			request.AddCookie(&http.Cookie{Name: handler.RefreshCookieName, Value: "private-refresh"})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			// 2. 核对完整 JSON、缓存/正文头及请求 ID 的上下文、响应和日志关联。
			if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" || response.Header().Get("Content-Type") != "application/json; charset=utf-8" || response.Header().Get("Content-Length") != strconv.Itoa(response.Body.Len()) || response.Header().Get("Content-Encoding") != "" || len(response.Header().Values("Set-Cookie")) != 0 {
				t.Fatal("成功状态或响应头错误")
			}
			var body dto.GlobalEntryPageResponse
			if json.Unmarshal(response.Body.Bytes(), &body) != nil || !reflect.DeepEqual(body.Data, *expected) || body.RequestID == "" || body.RequestID != storedID || body.RequestID != response.Header().Get("X-Request-ID") {
				t.Fatal("完整全局 JSON 或请求 ID 错误")
			}
			parsedID, err := uuid.Parse(body.RequestID)
			if err != nil || parsedID.Version() != 4 || (testCase.expectedID != "" && body.RequestID != testCase.expectedID) {
				t.Fatal("请求 ID 保留、生成或规范化错误")
			}
			var fields map[string]json.RawMessage
			if json.Unmarshal(response.Body.Bytes(), &fields) != nil || len(fields) != 2 {
				t.Fatal("响应外层字段错误")
			}
			var dataFields map[string]json.RawMessage
			if json.Unmarshal(fields["data"], &dataFields) != nil || len(dataFields) != 3 {
				t.Fatal("全局数据字段错误")
			}
			if expected.NextCursor == nil && string(dataFields["next_cursor"]) != "null" {
				t.Fatal("末页游标 null 错误")
			}
			if len(expected.Items) == 0 && string(dataFields["items"]) != "[]" {
				t.Fatal("空页数组错误")
			}
			var completed map[string]any
			if json.Unmarshal(logs.Bytes(), &completed) != nil || completed["request_id"] != body.RequestID || completed["status"] != float64(200) || completed["path"] != "/test-global-entry-page-success" || completed["method"] != "GET" {
				t.Fatal("请求完成日志错误")
			}
			if testCase.origin && response.Header().Get("Access-Control-Allow-Origin") != origin {
				t.Fatal("CORS 响应头缺失")
			}

			// 3. 核对请求值保护和来源结果保留。
			for _, marker := range []string{"private-query", "private-access", "private-refresh", "private-request-id"} {
				if strings.Contains(response.Body.String(), marker) || strings.Contains(logs.String(), marker) {
					t.Fatal("正文或完成日志包含原始请求值")
				}
			}
			if strings.Contains(logs.String(), "fixture-signed-next-cursor") {
				t.Fatal("完成日志包含下一页游标")
			}
			after, err := json.Marshal(result)
			if err != nil || !bytes.Equal(original, after) {
				t.Fatal("交付修改了来源结果")
			}
		})
	}
}

func TestGlobalEntryPageSuccessFailures(t *testing.T) {
	for _, failure := range []string{"nil result", "nil parent", "mixed owners", "unsafe bytes", "invalid time", "empty cursor", "partial invalid result", "cancelled", "deadline exceeded", "cancel before write"} {
		t.Run(failure, func(t *testing.T) {
			result := globalEntryPageSuccessFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var callContext context.Context = ctx
			switch failure {
			case "nil result":
				result = nil
			case "nil parent":
				result.Entries[0].ParentID = nil
			case "mixed owners":
				entry := result.Entries[0]
				entry.ID, entry.UserID = 24, 8
				result.Entries = append(result.Entries, entry)
			case "unsafe bytes":
				result.Entries[0].FileSize = 1 << 53
			case "invalid time":
				result.Entries[0].ModifiedAt = time.Time{}
			case "empty cursor":
				cursor := ""
				result.NextCursor = &cursor
			case "partial invalid result":
				invalid := result.Entries[0]
				invalid.ID = 24
				invalid.UpdatedAt = time.Time{}
				result.Entries = append(result.Entries, invalid)
			case "cancelled":
				cancel()
			case "deadline exceeded":
				expired, release := context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer release()
				callContext = expired
			}
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.GET("/test-global-entry-page-success", func(requestContext *gin.Context) {
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
				requestContext.Header("Content-Type", "application/octet-stream")
				if failure == "cancel before write" {
					lateContext, cancelLate := context.WithCancel(requestContext.Request.Context())
					defer cancelLate()
					requestContext.Request = requestContext.Request.WithContext(&cancelBeforeRefreshWriteContext{Context: lateContext, cancel: cancelLate})
				}
				handler.GlobalEntryPageSuccess(requestContext, result)
			}, func(*gin.Context) { t.Error("失败交付后进入下游") })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest("GET", "/test-global-entry-page-success", nil).WithContext(callContext))
			var body dto.ErrorResponse
			if json.Unmarshal(response.Body.Bytes(), &body) != nil || response.Code != 500 || body.Error.Code != "INTERNAL_ERROR" || body.Error.Details != nil || body.RequestID == "" || body.RequestID != response.Header().Get("X-Request-ID") || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" || response.Header().Get("Content-Type") != "application/json; charset=utf-8" || response.Header().Get("Content-Encoding") != "" || response.Header().Get("Content-Length") != "" || len(response.Header().Values("Set-Cookie")) != 0 {
				t.Fatal("异常交付的统一错误或响应头错误")
			}
		})
	}
}

func TestGlobalEntryPageSuccessCommittedResponse(t *testing.T) {
	for _, mode := range []string{"body written", "headers flushed"} {
		t.Run(mode, func(t *testing.T) {
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.GET("/test-global-entry-page-success", func(requestContext *gin.Context) {
				if mode == "body written" {
					requestContext.String(202, "already-written")
				} else {
					requestContext.Status(202)
					requestContext.Writer.WriteHeaderNow()
				}
				cancelled, cancel := context.WithCancel(requestContext.Request.Context())
				cancel()
				requestContext.Request = requestContext.Request.WithContext(cancelled)
				handler.GlobalEntryPageSuccess(requestContext, nil)
			}, func(*gin.Context) { t.Error("已提交响应后进入下游") })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest("GET", "/test-global-entry-page-success", nil))
			expectedBody, expectedType := "", ""
			if mode == "body written" {
				expectedBody, expectedType = "already-written", "text/plain; charset=utf-8"
			}
			if response.Code != 202 || response.Body.String() != expectedBody || response.Header().Get("Content-Type") != expectedType || response.Header().Get("Cache-Control") != "" || response.Header().Get("Pragma") != "" || len(response.Header().Values("Set-Cookie")) != 0 {
				t.Fatal("已提交响应被改写")
			}
		})
	}
}

func TestGlobalEntryPageSuccessHTTP(t *testing.T) {
	for _, mode := range []string{"entries next", "budget empty", "empty end", "entries end", "nil result"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 在系统分配的端口，通过两次真实 GET 验证同一结果与当前请求 ID。
			result := globalEntryPageSuccessFixture()
			switch mode {
			case "budget empty":
				result.Entries = nil
			case "empty end":
				result.Entries, result.NextCursor = nil, nil
			case "entries end":
				result.NextCursor = nil
			case "nil result":
				result = nil
			}
			var logs bytes.Buffer
			router := NewRouter(logging.New(&logs), nil, func(context.Context) bool { return true })
			router.GET("/test-global-entry-page-success", func(requestContext *gin.Context) { handler.GlobalEntryPageSuccess(requestContext, result) })
			server := httptest.NewServer(router)
			defer server.Close()
			client := server.Client()
			client.Timeout = 2 * time.Second
			for range 2 {
				requestID := uuid.NewString()
				request, err := http.NewRequest("GET", server.URL+"/test-global-entry-page-success?note=private-query", nil)
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("X-Request-ID", strings.ToUpper(requestID))
				response, err := client.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				encoded, readError := io.ReadAll(response.Body)
				closeError := response.Body.Close()
				if readError != nil || closeError != nil || response.Header.Get("Content-Type") != "application/json; charset=utf-8" || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Pragma") != "no-cache" || response.Header.Get("X-Request-ID") != requestID || response.Header.Get("Content-Encoding") != "" || response.ContentLength != int64(len(encoded)) || len(response.Cookies()) != 0 {
					t.Fatal("实际全局交付的响应头错误", readError, closeError)
				}
				if result == nil {
					var body dto.ErrorResponse
					if response.StatusCode != 500 || json.Unmarshal(encoded, &body) != nil || body.RequestID != requestID || body.Error.Code != "INTERNAL_ERROR" || body.Error.Details != nil {
						t.Fatal("实际失败响应错误")
					}
					continue
				}
				var body dto.GlobalEntryPageResponse
				expected, err := dto.NewGlobalEntryPageData(result)
				if err != nil || response.StatusCode != 200 || json.Unmarshal(encoded, &body) != nil || body.RequestID != requestID || !reflect.DeepEqual(body.Data, *expected) {
					t.Fatal("实际全局正文或请求 ID 错误", err)
				}
			}
			server.Close()
			if strings.Contains(logs.String(), "private-query") || strings.Contains(logs.String(), "fixture-signed-next-cursor") || strings.Contains(logs.String(), "学习笔记.md") {
				t.Fatal("实际完成日志包含查询、游标或业务原文")
			}
		})
	}
}
