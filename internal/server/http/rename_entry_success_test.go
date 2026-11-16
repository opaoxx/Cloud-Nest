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
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

func renameEntrySuccessFixture(t *testing.T) *dto.RenameEntryData {
	t.Helper()
	result := entryDetailSuccessFixture()
	target := &result.Path.Entries[len(result.Path.Entries)-1]
	target.Name, target.Revision = "学习笔记.PDF", 4
	target.ModifiedAt, target.UpdatedAt = target.ModifiedAt.Add(24*time.Hour), target.UpdatedAt.Add(24*time.Hour)
	extension := "pdf"
	result.Display.Extension = &extension
	data, err := dto.NewRenameEntryData(result)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestRenameEntrySuccessDelivery(t *testing.T) {
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	for _, testCase := range []struct {
		name       string
		requestIDs []string
		expectedID string
	}{
		{name: "existing id", requestIDs: []string{requestID}, expectedID: requestID},
		{name: "uppercase id", requestIDs: []string{strings.ToUpper(requestID)}, expectedID: requestID},
		{name: "generated id"}, {name: "invalid id", requestIDs: []string{"private-request-id"}},
		{name: "duplicate id", requestIDs: []string{requestID, requestID}},
		{name: "allowed origin"}, {name: "pending body headers"}, {name: "folder"}, {name: "null fields"},
		{name: "empty text"}, {name: "maximum integers"}, {name: "minimum time"}, {name: "maximum time"},
		{name: "reformatted json"}, {name: "escaped name"}, {name: "zero bytes"}, {name: "revision one"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 准备稳定快照，通过公共中间件和测试 PATCH 入口交付。
			data := renameEntrySuccessFixture(t)
			entry := &data.Entry
			switch testCase.name {
			case "folder":
				entry.Kind, entry.SizeBytes = "folder", 0
				entry.Extension, entry.MIMEType, entry.SourceDevice, entry.ContentModifiedAt = nil, nil, nil, nil
				color := "green"
				entry.ColorKey = &color
			case "null fields":
				entry.Extension, entry.MIMEType, entry.ColorKey, entry.SourceDevice, entry.ContentModifiedAt = nil, nil, nil, nil, nil
				entry.Starred, entry.StarredAt, entry.LastOpenedAt = false, nil, nil
			case "empty text":
				empty := ""
				entry.Extension, entry.MIMEType, entry.ColorKey, entry.SourceDevice.Name = &empty, &empty, &empty, &empty
			case "maximum integers":
				parent, source := "18446744073709551614", "18446744073709551615"
				entry.ID, entry.ParentID, entry.Revision, entry.SourceDevice.ID = source, &parent, source, &source
				entry.SizeBytes = 9007199254740991
			case "minimum time", "maximum time":
				stamp := time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)
				if testCase.name == "maximum time" {
					stamp = time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
				}
				entry.CreatedAt, entry.ModifiedAt, entry.UpdatedAt = stamp, stamp, stamp
				*entry.ContentModifiedAt, *entry.StarredAt, *entry.LastOpenedAt = stamp, stamp, stamp
			case "escaped name":
				entry.Name = "Cafe\u0301 <学习>&笔记 🌱"
			case "zero bytes":
				entry.SizeBytes = 0
			case "revision one":
				entry.Revision = "1"
			}
			stableBody, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			if testCase.name == "reformatted json" {
				var formatted bytes.Buffer
				if err := json.Indent(&formatted, stableBody, "", "  "); err != nil {
					t.Fatal(err)
				}
				stableBody = formatted.Bytes()
			}
			original := bytes.Clone(stableBody)
			var logs bytes.Buffer
			const origin = "http://127.0.0.1:5173"
			router := NewRouter(logging.New(&logs), []string{origin}, func(context.Context) bool { return true })
			var storedID string
			router.PATCH("/test-rename-entry-success", func(requestContext *gin.Context) {
				storedID = requestcontext.RequestID(requestContext.Request.Context())
				if testCase.name == "pending body headers" {
					requestContext.Header("Content-Length", "3")
					requestContext.Header("Content-Encoding", "gzip")
					requestContext.Header("Content-Type", "application/octet-stream")
				}
				handler.RenameEntrySuccess(requestContext, stableBody)
			}, func(*gin.Context) { t.Error("成功交付后进入下游") })
			request := httptest.NewRequest("PATCH", "/test-rename-entry-success?note=private-query", strings.NewReader("private-body"))
			for _, value := range testCase.requestIDs {
				request.Header.Add("X-Request-ID", value)
			}
			if testCase.name == "allowed origin" {
				request.Header.Set("Origin", origin)
			}
			request.Header.Set("Authorization", "Bearer private-access")
			request.Header.Set("Idempotency-Key", "private-key")
			request.AddCookie(&http.Cookie{Name: handler.RefreshCookieName, Value: "private-refresh"})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			// 2. 核对完整快照、当前请求 ID、状态及响应头。
			if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" ||
				response.Header().Get("Content-Type") != "application/json; charset=utf-8" || response.Header().Get("Content-Length") != strconv.Itoa(response.Body.Len()) ||
				response.Header().Get("Content-Encoding") != "" || len(response.Header().Values("Set-Cookie")) != 0 {
				t.Fatal("成功状态或响应头错误")
			}
			var body dto.RenameEntryResponse
			if json.Unmarshal(response.Body.Bytes(), &body) != nil || !reflect.DeepEqual(body.Data, *data) || body.RequestID == "" || body.RequestID != storedID || body.RequestID != response.Header().Get("X-Request-ID") {
				t.Fatal("成功快照或请求关联错误")
			}
			parsedID, err := uuid.Parse(body.RequestID)
			if err != nil || parsedID.Version() != 4 || (testCase.expectedID != "" && body.RequestID != testCase.expectedID) {
				t.Fatal("请求 ID 生成或规范化错误")
			}
			var outer map[string]json.RawMessage
			var payload map[string]json.RawMessage
			var fields map[string]json.RawMessage
			if json.Unmarshal(response.Body.Bytes(), &outer) != nil || len(outer) != 2 || json.Unmarshal(outer["data"], &payload) != nil || len(payload) != 1 || json.Unmarshal(payload["entry"], &fields) != nil || len(fields) != 18 {
				t.Fatal("响应字段形状错误")
			}

			// 3. 核对完成日志、跨域响应和稳定输入。
			var completed map[string]any
			if json.Unmarshal(logs.Bytes(), &completed) != nil || completed["request_id"] != body.RequestID || completed["status"] != float64(200) || completed["method"] != "PATCH" || completed["path"] != "/test-rename-entry-success" || completed["level"] != "INFO" {
				t.Fatal("完成日志错误")
			}
			if testCase.name == "allowed origin" && (response.Header().Get("Access-Control-Allow-Origin") != origin || response.Header().Get("Access-Control-Allow-Credentials") != "true") {
				t.Fatal("跨域响应头错误")
			}
			for _, marker := range []string{"private-query", "private-body", "private-access", "private-key", "private-refresh", "private-request-id"} {
				if strings.Contains(response.Body.String(), marker) || strings.Contains(logs.String(), marker) {
					t.Fatal("正文或日志包含原始请求值")
				}
			}
			if strings.Contains(logs.String(), data.Entry.Name) || !bytes.Equal(stableBody, original) {
				t.Fatal("日志包含展示名称或稳定输入被修改")
			}
		})
	}
}

func TestRenameEntrySuccessFailures(t *testing.T) {
	for _, mode := range []string{"nil data", "invalid json", "null", "array", "invalid utf8", "missing field", "unknown field", "numeric id", "zero revision", "source shape", "starred mismatch", "invalid time", "folder size", "cancelled", "deadline", "cancel before write"} {
		t.Run(mode, func(t *testing.T) {
			stableBody, err := json.Marshal(renameEntrySuccessFixture(t))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var callContext context.Context = ctx
			switch mode {
			case "nil data":
				stableBody = nil
			case "invalid json":
				stableBody = []byte("private-invalid-json")
			case "null", "array":
				stableBody = []byte("null")
				if mode == "array" {
					stableBody = []byte("[]")
				}
			case "invalid utf8":
				stableBody = bytes.Replace(stableBody, []byte("学习笔记"), []byte{0xff}, 1)
			case "cancelled":
				cancel()
			case "deadline":
				expired, release := context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer release()
				callContext = expired
			case "cancel before write":
			default:
				var payload map[string]map[string]any
				if err := json.Unmarshal(stableBody, &payload); err != nil {
					t.Fatal(err)
				}
				entry := payload["entry"]
				switch mode {
				case "missing field":
					delete(entry, "extension")
				case "unknown field":
					entry["private_field"] = true
				case "numeric id":
					entry["id"] = 23
				case "zero revision":
					entry["revision"] = "0"
				case "source shape":
					entry["source_device"] = map[string]any{"id": "19"}
				case "starred mismatch":
					entry["starred"] = false
				case "invalid time":
					entry["created_at"] = "private-time"
				case "folder size":
					entry["kind"] = "folder"
				}
				stableBody, err = json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
			}
			var logs bytes.Buffer
			router := NewRouter(logging.New(&logs), nil, func(context.Context) bool { return true })
			router.PATCH("/test-rename-entry-success", func(requestContext *gin.Context) {
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
				requestContext.Header("Content-Type", "application/octet-stream")
				if mode == "cancel before write" {
					lateContext, cancelLate := context.WithCancel(requestContext.Request.Context())
					defer cancelLate()
					requestContext.Request = requestContext.Request.WithContext(&cancelBeforeRefreshWriteContext{Context: lateContext, cancel: cancelLate})
				}
				handler.RenameEntrySuccess(requestContext, stableBody)
			}, func(*gin.Context) { t.Error("失败交付后进入下游") })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest("PATCH", "/test-rename-entry-success?note=private-query", nil).WithContext(callContext))
			var body dto.ErrorResponse
			if json.Unmarshal(response.Body.Bytes(), &body) != nil || response.Code != 500 || body.Error.Code != "INTERNAL_ERROR" || body.Error.Message != "服务内部错误，请稍后重试" || body.Error.Details != nil || body.RequestID == "" || body.RequestID != response.Header().Get("X-Request-ID") ||
				response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" || response.Header().Get("Content-Type") != "application/json; charset=utf-8" || response.Header().Get("Content-Encoding") != "" || response.Header().Get("Content-Length") != "" || len(response.Header().Values("Set-Cookie")) != 0 {
				t.Fatal("失败响应或头信息错误")
			}
			var completed map[string]any
			if json.Unmarshal(logs.Bytes(), &completed) != nil || completed["status"] != float64(500) || completed["level"] != "ERROR" || completed["request_id"] != body.RequestID {
				t.Fatal("失败完成日志错误")
			}
			for _, marker := range []string{"private-query", "private-invalid-json", "private_field", "private-time", "学习笔记"} {
				if strings.Contains(response.Body.String(), marker) || strings.Contains(logs.String(), marker) {
					t.Fatal("失败正文或日志包含内部数据")
				}
			}
		})
	}
}

func TestRenameEntrySuccessCommittedResponse(t *testing.T) {
	for _, mode := range []string{"body written", "headers flushed"} {
		t.Run(mode, func(t *testing.T) {
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.PATCH("/test-rename-entry-success", func(requestContext *gin.Context) {
				if mode == "body written" {
					requestContext.String(202, "already-written")
				} else {
					requestContext.Status(202)
					requestContext.Writer.WriteHeaderNow()
				}
				cancelled, cancel := context.WithCancel(requestContext.Request.Context())
				cancel()
				requestContext.Request = requestContext.Request.WithContext(cancelled)
				handler.RenameEntrySuccess(requestContext, nil)
			}, func(*gin.Context) { t.Error("已提交响应后进入下游") })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest("PATCH", "/test-rename-entry-success", nil))
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

func TestRenameEntrySuccessHTTP(t *testing.T) {
	for _, mode := range []string{"saved snapshot", "null fields", "invalid data"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 使用空闲端口，以实际 PATCH 验证成功与异常传输。
			data := renameEntrySuccessFixture(t)
			if mode == "null fields" {
				data.Entry.Extension, data.Entry.MIMEType, data.Entry.ColorKey, data.Entry.SourceDevice = nil, nil, nil, nil
				data.Entry.ContentModifiedAt, data.Entry.StarredAt, data.Entry.LastOpenedAt, data.Entry.Starred = nil, nil, nil, false
			}
			stableBody, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			status := 200
			if mode == "invalid data" {
				stableBody, status = []byte(`{"entry":null}`), 500
			}
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.PATCH("/test-rename-entry-success", func(requestContext *gin.Context) {
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
				handler.RenameEntrySuccess(requestContext, stableBody)
			})
			server := httptest.NewServer(router)
			defer server.Close()
			client := server.Client()
			client.Timeout = 2 * time.Second
			var previousID string
			for _, requestID := range []string{"28EE758A-0727-42DE-AE7D-3BB33155E148", "28EE758A-0727-42DE-AE7D-3BB33155E149"} {
				request, err := http.NewRequest("PATCH", server.URL+"/test-rename-entry-success", nil)
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("X-Request-ID", requestID)
				response, err := client.Do(request)
				if err != nil {
					t.Fatal("真实 PATCH 请求失败", err)
				}
				encoded, readError := io.ReadAll(response.Body)
				closeError := response.Body.Close()
				if readError != nil || closeError != nil || response.StatusCode != status || response.ContentLength != int64(len(encoded)) || response.Header.Get("Content-Type") != "application/json; charset=utf-8" || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Pragma") != "no-cache" || response.Header.Get("Content-Encoding") != "" || len(response.Cookies()) != 0 {
					t.Fatal("真实响应状态、正文或头信息错误", readError, closeError)
				}
				// 2. 同一稳定快照关联每次请求自己的 ID。
				currentID := response.Header.Get("X-Request-ID")
				if currentID != strings.ToLower(requestID) || currentID == previousID {
					t.Fatal("响应未关联当前请求 ID")
				}
				previousID = currentID
				if status == 200 {
					var body dto.RenameEntryResponse
					if json.Unmarshal(encoded, &body) != nil || !reflect.DeepEqual(body.Data, *data) || body.RequestID != currentID {
						t.Fatal("真实成功 JSON 错误")
					}
				} else {
					var body dto.ErrorResponse
					if json.Unmarshal(encoded, &body) != nil || body.Error.Code != "INTERNAL_ERROR" || body.Error.Details != nil || body.RequestID != currentID {
						t.Fatal("真实错误 JSON 错误")
					}
				}
			}
		})
	}
}
