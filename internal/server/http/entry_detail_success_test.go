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

func entryDetailSuccessFixture() *domain.EntryDetail {
	page := directoryPageSuccessFixture()
	entries := append(page.Path.Entries, page.Entries[0])
	for index := range entries {
		entries[index].CreatedAt = page.Entries[0].CreatedAt
		entries[index].ModifiedAt = page.Entries[0].ModifiedAt
		entries[index].UpdatedAt = page.Entries[0].UpdatedAt
	}
	category, extension, mimeType := "DOCUMENT", "md", "text/markdown"
	sourceID, sourceName := uint64(19), "发布时设备 🎮"
	contentAt := page.Entries[0].CreatedAt
	starredAt, openedAt := page.Entries[0].ModifiedAt, page.Entries[0].ModifiedAt.Add(time.Hour)
	return &domain.EntryDetail{
		Path: domain.FileEntryPath{Entries: entries},
		Display: domain.FileEntryDisplay{
			ID: 23, UserID: 7, Category: &category, Extension: &extension, MIMEType: &mimeType,
			SourceDeviceID: &sourceID, SourceDeviceName: &sourceName, ContentModifiedAt: &contentAt,
		},
		Preference: domain.EntryPreference{UserID: 7, EntryID: 23, StarredAt: &starredAt, LastOpenedAt: &openedAt},
	}
}

func TestEntryDetailSuccessDelivery(t *testing.T) {
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
		{name: "directory"},
		{name: "root"},
		{name: "missing source"},
		{name: "source name only"},
		{name: "source id only"},
		{name: "default preference"},
		{name: "maximum ids"},
		{name: "safe byte boundary"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 测试 GET 入口交付详情，复用公共中间件及已完成的 DTO。
			result := entryDetailSuccessFixture()
			switch testCase.name {
			case "directory", "root":
				result.Path.Entries = result.Path.Entries[:2]
				if testCase.name == "root" {
					result.Path.Entries = result.Path.Entries[:1]
				}
				target := result.Path.Entries[len(result.Path.Entries)-1]
				color := "sand"
				result.Display = domain.FileEntryDisplay{ID: target.ID, UserID: target.UserID, ColorKey: &color}
				result.Preference = domain.EntryPreference{UserID: target.UserID, EntryID: target.ID}
			case "missing source":
				result.Display.SourceDeviceID, result.Display.SourceDeviceName = nil, nil
			case "source name only":
				result.Display.SourceDeviceID = nil
			case "source id only":
				result.Display.SourceDeviceName = nil
			case "default preference":
				result.Preference.StarredAt, result.Preference.LastOpenedAt = nil, nil
			case "maximum ids":
				maximum := ^uint64(0)
				for index := range result.Path.Entries {
					entry := &result.Path.Entries[index]
					entry.ID, entry.UserID = maximum-2+uint64(index), maximum
					if index > 0 {
						parent := result.Path.Entries[index-1].ID
						entry.ParentID = &parent
					}
				}
				result.Path.Entries[2].Revision = maximum
				result.Display.ID, result.Display.UserID, result.Display.SourceDeviceID = maximum, maximum, &maximum
				result.Preference.UserID, result.Preference.EntryID = maximum, maximum
			case "safe byte boundary":
				result.Path.Entries[2].FileSize = 1<<53 - 1
			}
			original, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			expected, err := dto.NewEntryDetailData(result)
			if err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			var storedID string
			origin := "http://127.0.0.1:5173"
			router := NewRouter(logging.New(&logs), []string{origin}, func(context.Context) bool { return true })
			router.GET("/test-entry-detail-success", func(requestContext *gin.Context) {
				storedID = requestcontext.RequestID(requestContext.Request.Context())
				if testCase.pending {
					requestContext.Header("Content-Length", "3")
					requestContext.Header("Content-Encoding", "gzip")
					requestContext.Header("Content-Type", "application/octet-stream")
				}
				handler.EntryDetailSuccess(requestContext, result)
			}, func(*gin.Context) { t.Error("详情交付后进入下游") })
			request := httptest.NewRequest("GET", "/test-entry-detail-success?note=private-query", nil)
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

			// 2. 核对完整正文、缓存及正文头，以及请求 ID 的上下文和日志关联。
			if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" ||
				response.Header().Get("Content-Type") != "application/json; charset=utf-8" || response.Header().Get("Content-Length") != strconv.Itoa(response.Body.Len()) ||
				response.Header().Get("Content-Encoding") != "" || len(response.Header().Values("Set-Cookie")) != 0 {
				t.Fatal("详情成功状态或响应头错误")
			}
			var body dto.EntryDetailResponse
			if json.Unmarshal(response.Body.Bytes(), &body) != nil || !reflect.DeepEqual(body.Data, *expected) || body.RequestID == "" || body.RequestID != storedID || body.RequestID != response.Header().Get("X-Request-ID") {
				t.Fatal("完整详情 JSON 或请求关联错误")
			}
			parsedID, err := uuid.Parse(body.RequestID)
			if err != nil || parsedID.Version() != 4 || (testCase.expectedID != "" && body.RequestID != testCase.expectedID) {
				t.Fatal("详情请求 ID 生成或规范化错误")
			}
			var fields map[string]json.RawMessage
			var dataFields map[string]json.RawMessage
			if json.Unmarshal(response.Body.Bytes(), &fields) != nil || len(fields) != 2 || json.Unmarshal(fields["data"], &dataFields) != nil || len(dataFields) != 18 {
				t.Fatal("详情响应字段数量错误")
			}
			if (expected.ParentID == nil && string(dataFields["parent_id"]) != "null") || (expected.SourceDevice == nil && string(dataFields["source_device"]) != "null") ||
				(expected.StarredAt == nil && string(dataFields["starred_at"]) != "null") || (expected.LastOpenedAt == nil && string(dataFields["last_opened_at"]) != "null") {
				t.Fatal("详情可空字段编码错误")
			}
			var completed map[string]any
			if json.Unmarshal(logs.Bytes(), &completed) != nil || completed["request_id"] != body.RequestID || completed["status"] != float64(200) || completed["path"] != "/test-entry-detail-success" || completed["method"] != "GET" {
				t.Fatal("详情请求完成日志错误")
			}
			if testCase.origin && response.Header().Get("Access-Control-Allow-Origin") != origin {
				t.Fatal("详情 CORS 响应头缺失")
			}

			// 3. 检查原始请求和展示正文的日志保护，并核对来源数据保留。
			for _, marker := range []string{"private-query", "private-access", "private-refresh", "private-request-id"} {
				if strings.Contains(response.Body.String(), marker) || strings.Contains(logs.String(), marker) {
					t.Fatal("详情正文或日志包含原始请求值")
				}
			}
			for _, marker := range []string{"学习笔记.md", "课程资料 🌱", "发布时设备 🎮"} {
				if strings.Contains(logs.String(), marker) {
					t.Fatal("完成日志包含详情展示正文")
				}
			}
			after, err := json.Marshal(result)
			if err != nil || !bytes.Equal(original, after) {
				t.Fatal("交付修改了内部详情")
			}
		})
	}
}

func TestEntryDetailSuccessFailures(t *testing.T) {
	for _, failure := range []string{
		"nil result", "missing path", "wrong owner", "unsafe bytes", "invalid base time", "unknown category",
		"zero source", "invalid preference time", "invalid content time", "cancelled", "deadline exceeded", "cancel before write",
	} {
		t.Run(failure, func(t *testing.T) {
			result := entryDetailSuccessFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var callContext context.Context = ctx
			switch failure {
			case "nil result":
				result = nil
			case "missing path":
				result.Path.Entries = nil
			case "wrong owner":
				result.Display.UserID = 8
			case "unsafe bytes":
				result.Path.Entries[2].FileSize = 1 << 53
			case "invalid base time":
				result.Path.Entries[2].ModifiedAt = time.Time{}
			case "unknown category":
				*result.Display.Category = "private-category"
			case "zero source":
				*result.Display.SourceDeviceID = 0
			case "invalid preference time":
				*result.Preference.LastOpenedAt = time.Time{}
			case "invalid content time":
				*result.Display.ContentModifiedAt = time.Time{}
			case "cancelled":
				cancel()
			case "deadline exceeded":
				expired, release := context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer release()
				callContext = expired
			}
			var logs bytes.Buffer
			router := NewRouter(logging.New(&logs), nil, func(context.Context) bool { return true })
			router.GET("/test-entry-detail-success", func(requestContext *gin.Context) {
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
				requestContext.Header("Content-Type", "application/octet-stream")
				if failure == "cancel before write" {
					lateContext, cancelLate := context.WithCancel(requestContext.Request.Context())
					defer cancelLate()
					requestContext.Request = requestContext.Request.WithContext(&cancelBeforeRefreshWriteContext{Context: lateContext, cancel: cancelLate})
				}
				handler.EntryDetailSuccess(requestContext, result)
			}, func(*gin.Context) { t.Error("失败交付后进入下游") })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest("GET", "/test-entry-detail-success?note=private-query", nil).WithContext(callContext))
			var body dto.ErrorResponse
			if json.Unmarshal(response.Body.Bytes(), &body) != nil || response.Code != 500 || body.Error.Code != "INTERNAL_ERROR" || body.Error.Message != "服务内部错误，请稍后重试" || body.Error.Details != nil ||
				body.RequestID == "" || body.RequestID != response.Header().Get("X-Request-ID") || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" ||
				response.Header().Get("Content-Type") != "application/json; charset=utf-8" || response.Header().Get("Content-Encoding") != "" || response.Header().Get("Content-Length") != "" || len(response.Header().Values("Set-Cookie")) != 0 {
				t.Fatal("详情失败响应的统一错误或正文头错误")
			}
			var completed map[string]any
			if json.Unmarshal(logs.Bytes(), &completed) != nil || completed["request_id"] != body.RequestID || completed["status"] != float64(500) {
				t.Fatal("详情失败完成日志关联错误")
			}
			for _, marker := range []string{"private-query", "private-category", "学习笔记.md", "发布时设备 🎮"} {
				if strings.Contains(response.Body.String(), marker) || strings.Contains(logs.String(), marker) {
					t.Fatal("失败正文或日志包含原始数据")
				}
			}
		})
	}
}

func TestEntryDetailSuccessCommittedResponse(t *testing.T) {
	for _, mode := range []string{"body written", "headers flushed"} {
		t.Run(mode, func(t *testing.T) {
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.GET("/test-entry-detail-success", func(requestContext *gin.Context) {
				if mode == "body written" {
					requestContext.String(202, "already-written")
				} else {
					requestContext.Status(202)
					requestContext.Writer.WriteHeaderNow()
				}
				cancelled, cancel := context.WithCancel(requestContext.Request.Context())
				cancel()
				requestContext.Request = requestContext.Request.WithContext(cancelled)
				handler.EntryDetailSuccess(requestContext, nil)
			}, func(*gin.Context) { t.Error("已提交响应后进入下游") })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest("GET", "/test-entry-detail-success", nil))
			expectedBody, expectedType := "", ""
			if mode == "body written" {
				expectedBody, expectedType = "already-written", "text/plain; charset=utf-8"
			}
			if response.Code != 202 || response.Body.String() != expectedBody || response.Header().Get("Content-Type") != expectedType || response.Header().Get("Cache-Control") != "" || response.Header().Get("Pragma") != "" || len(response.Header().Values("Set-Cookie")) != 0 {
				t.Fatal("已提交响应被修改")
			}
		})
	}
}

func TestEntryDetailSuccessHTTP(t *testing.T) {
	for _, mode := range []string{"file", "root", "invalid data"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 在空闲端口通过实际 GET 请求验证成功与错误传输。
			result := entryDetailSuccessFixture()
			status := http.StatusOK
			if mode == "root" {
				result.Path.Entries = result.Path.Entries[:1]
				result.Display = domain.FileEntryDisplay{ID: 11, UserID: 7}
				result.Preference = domain.EntryPreference{UserID: 7, EntryID: 11}
			}
			if mode == "invalid data" {
				result.Display.UserID, status = 8, http.StatusInternalServerError
			}
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.GET("/test-entry-detail-success", func(requestContext *gin.Context) {
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
				requestContext.Header("Content-Type", "application/octet-stream")
				handler.EntryDetailSuccess(requestContext, result)
			})
			server := httptest.NewServer(router)
			defer server.Close()
			client := server.Client()
			client.Timeout = 2 * time.Second
			request, err := http.NewRequest("GET", server.URL+"/test-entry-detail-success", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("X-Request-ID", "28EE758A-0727-42DE-AE7D-3BB33155E148")
			response, err := client.Do(request)
			if err != nil {
				t.Fatal("真实详情 HTTP 请求失败", err)
			}
			defer response.Body.Close()
			encoded, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != status || response.ContentLength != int64(len(encoded)) || response.Header.Get("Content-Type") != "application/json; charset=utf-8" ||
				response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Pragma") != "no-cache" || response.Header.Get("Content-Encoding") != "" || len(response.Cookies()) != 0 {
				t.Fatal("真实详情状态、正文或响应头错误", err)
			}

			// 2. 核对完整 DTO、统一错误及实际响应中的请求 ID。
			requestID := response.Header.Get("X-Request-ID")
			if requestID != "28ee758a-0727-42de-ae7d-3bb33155e148" {
				t.Fatal("真实详情请求 ID 错误")
			}
			if status == http.StatusOK {
				var body dto.EntryDetailResponse
				expected, err := dto.NewEntryDetailData(result)
				if err != nil || json.Unmarshal(encoded, &body) != nil || !reflect.DeepEqual(body.Data, *expected) || body.RequestID != requestID {
					t.Fatal("真实详情 JSON 字段错误", err)
				}
			} else {
				var body dto.ErrorResponse
				if json.Unmarshal(encoded, &body) != nil || body.Error.Code != "INTERNAL_ERROR" || body.Error.Details != nil || body.RequestID != requestID {
					t.Fatal("真实详情统一错误字段错误")
				}
			}
		})
	}
}
