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

func directoryPageSuccessFixture() *domain.DirectoryPageResult {
	rootID, parentID := uint64(11), uint64(12)
	stamp := time.Date(2026, 11, 15, 9, 30, 0, 123456789, time.FixedZone("fixture", 8*3600))
	root := domain.FileEntry{ID: rootID, UserID: 7, EntryType: "DIR", Revision: 1, Status: "ACTIVE"}
	directory := root
	directory.ID, directory.ParentID, directory.Name = parentID, &rootID, "课程资料 🌱"
	file := domain.FileEntry{ID: 23, UserID: 7, ParentID: &parentID, Name: "学习笔记.md", EntryType: "FILE", FileSize: 2048, Revision: 3, Status: "ACTIVE", CreatedAt: stamp, ModifiedAt: stamp.Add(time.Hour), UpdatedAt: stamp.Add(time.Hour)}
	cursor := "fixture-signed-next-cursor"
	return &domain.DirectoryPageResult{Path: domain.FileEntryPath{Entries: []domain.FileEntry{root, directory}}, Entries: []domain.FileEntry{file}, NextCursor: &cursor}
}

func TestDirectoryPageSuccessDelivery(t *testing.T) {
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
		{name: "root page"},
		{name: "maximum ids"},
		{name: "safe byte boundary"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 测试专用 GET 入口交付成功目录页，复用公共中间件。
			result := directoryPageSuccessFixture()
			switch testCase.name {
			case "empty page":
				result.Entries, result.NextCursor = nil, nil
			case "last page":
				result.NextCursor = nil
			case "root page":
				result.Path.Entries = result.Path.Entries[:1]
				result.Entries, result.NextCursor = nil, nil
			case "maximum ids":
				maximum := ^uint64(0)
				rootID, parentID := maximum-2, maximum-1
				result.Path.Entries[0].ID, result.Path.Entries[0].UserID = rootID, maximum
				result.Path.Entries[1].ID, result.Path.Entries[1].UserID, result.Path.Entries[1].ParentID = parentID, maximum, &rootID
				result.Entries[0].ID, result.Entries[0].UserID, result.Entries[0].ParentID, result.Entries[0].Revision = maximum, maximum, &parentID, maximum
			case "safe byte boundary":
				result.Entries[0].FileSize = 1<<53 - 1
			}
			original, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			expected, err := dto.NewDirectoryPageData(result)
			if err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			var storedID string
			origin := "http://127.0.0.1:5173"
			router := NewRouter(logging.New(&logs), []string{origin}, func(context.Context) bool { return true })
			router.GET("/test-directory-page-success", func(requestContext *gin.Context) {
				storedID = requestcontext.RequestID(requestContext.Request.Context())
				if testCase.pending {
					requestContext.Header("Content-Length", "3")
					requestContext.Header("Content-Encoding", "gzip")
					requestContext.Header("Content-Type", "application/octet-stream")
				}
				handler.DirectoryPageSuccess(requestContext, result)
			}, func(*gin.Context) { t.Error("成功交付后进入下游") })
			request := httptest.NewRequest("GET", "/test-directory-page-success?note=private-query", nil)
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
			var body dto.DirectoryPageResponse
			if json.Unmarshal(response.Body.Bytes(), &body) != nil || !reflect.DeepEqual(body.Data, *expected) || body.RequestID == "" || body.RequestID != storedID || body.RequestID != response.Header().Get("X-Request-ID") {
				t.Fatal("完整目录 JSON 或请求 ID 错误")
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
				t.Fatal("目录数据字段错误")
			}
			if expected.NextCursor == nil && string(dataFields["next_cursor"]) != "null" {
				t.Fatal("末页游标 null 错误")
			}
			if len(expected.Items) == 0 && string(dataFields["items"]) != "[]" {
				t.Fatal("空页数组错误")
			}
			var completed map[string]any
			if json.Unmarshal(logs.Bytes(), &completed) != nil || completed["request_id"] != body.RequestID || completed["status"] != float64(200) || completed["path"] != "/test-directory-page-success" || completed["method"] != "GET" {
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

func TestDirectoryPageSuccessFailures(t *testing.T) {
	for _, failure := range []string{"nil result", "missing path", "wrong owner", "unsafe bytes", "invalid time", "empty cursor", "partial invalid result", "cancelled", "deadline exceeded", "cancel before write"} {
		t.Run(failure, func(t *testing.T) {
			result := directoryPageSuccessFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var callContext context.Context = ctx
			switch failure {
			case "nil result":
				result = nil
			case "missing path":
				result.Path.Entries = nil
			case "wrong owner":
				result.Entries[0].UserID = 8
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
			router.GET("/test-directory-page-success", func(requestContext *gin.Context) {
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
				requestContext.Header("Content-Type", "application/octet-stream")
				if failure == "cancel before write" {
					lateContext, cancelLate := context.WithCancel(requestContext.Request.Context())
					defer cancelLate()
					requestContext.Request = requestContext.Request.WithContext(&cancelBeforeRefreshWriteContext{Context: lateContext, cancel: cancelLate})
				}
				handler.DirectoryPageSuccess(requestContext, result)
			}, func(*gin.Context) { t.Error("失败交付后进入下游") })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest("GET", "/test-directory-page-success", nil).WithContext(callContext))
			var body dto.ErrorResponse
			if json.Unmarshal(response.Body.Bytes(), &body) != nil || response.Code != 500 || body.Error.Code != "INTERNAL_ERROR" || body.Error.Details != nil || body.RequestID == "" || body.RequestID != response.Header().Get("X-Request-ID") || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" || response.Header().Get("Content-Type") != "application/json; charset=utf-8" || response.Header().Get("Content-Encoding") != "" || response.Header().Get("Content-Length") != "" || len(response.Header().Values("Set-Cookie")) != 0 {
				t.Fatal("异常交付的统一错误或响应头错误")
			}
		})
	}
}

func TestDirectoryPageSuccessCommittedResponse(t *testing.T) {
	for _, mode := range []string{"body written", "headers flushed"} {
		t.Run(mode, func(t *testing.T) {
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.GET("/test-directory-page-success", func(requestContext *gin.Context) {
				if mode == "body written" {
					requestContext.String(202, "already-written")
				} else {
					requestContext.Status(202)
					requestContext.Writer.WriteHeaderNow()
				}
				cancelled, cancel := context.WithCancel(requestContext.Request.Context())
				cancel()
				requestContext.Request = requestContext.Request.WithContext(cancelled)
				handler.DirectoryPageSuccess(requestContext, nil)
			}, func(*gin.Context) { t.Error("已提交响应后进入下游") })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest("GET", "/test-directory-page-success", nil))
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

func TestDirectoryPageSuccessHTTP(t *testing.T) {
	// 1. 使用系统分配的空闲端口验证实际 GET 传输。
	result := directoryPageSuccessFixture()
	router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
	router.GET("/test-directory-page-success", func(requestContext *gin.Context) { handler.DirectoryPageSuccess(requestContext, result) })
	server := httptest.NewServer(router)
	defer server.Close()
	client := server.Client()
	client.Timeout = 2 * time.Second
	request, err := http.NewRequest("GET", server.URL+"/test-directory-page-success", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Request-ID", "28EE758A-0727-42DE-AE7D-3BB33155E148")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("真实目录页 HTTP 请求失败", err)
	}
	defer response.Body.Close()
	encoded, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}

	// 2. 核对网络响应的完整数据、长度、请求 ID 和缓存头。
	var body dto.DirectoryPageResponse
	expected, err := dto.NewDirectoryPageData(result)
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(encoded, &body) != nil || response.StatusCode != 200 || !reflect.DeepEqual(body.Data, *expected) || body.RequestID != "28ee758a-0727-42de-ae7d-3bb33155e148" || body.RequestID != response.Header.Get("X-Request-ID") || response.ContentLength != int64(len(encoded)) || response.Header.Get("Content-Type") != "application/json; charset=utf-8" || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Pragma") != "no-cache" || response.Header.Get("Content-Encoding") != "" || len(response.Cookies()) != 0 {
		t.Fatal("真实目录页交付错误")
	}
}
