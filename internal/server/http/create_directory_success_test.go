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

func createDirectorySuccessFixture(t *testing.T) *dto.CreateDirectoryData {
	t.Helper()
	parentID := uint64(11)
	color := "green"
	stamp := time.Date(2026, 11, 16, 9, 30, 0, 123456789, time.FixedZone("fixture", 8*3600))
	data, err := dto.NewCreateDirectoryData(&domain.FileEntry{
		ID: 42, UserID: 7, ParentID: &parentID, Name: "学习资料 🌱", EntryType: "DIR", Revision: 1, Status: "ACTIVE",
		CreatedAt: stamp, ModifiedAt: stamp.Add(time.Second), UpdatedAt: stamp.Add(2 * time.Second),
	}, &color)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestCreateDirectorySuccessDelivery(t *testing.T) {
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	for _, testCase := range []struct {
		name       string
		requestIDs []string
		expectedID string
	}{
		{name: "existing request id", requestIDs: []string{requestID}, expectedID: requestID},
		{name: "uppercase request id", requestIDs: []string{strings.ToUpper(requestID)}, expectedID: requestID},
		{name: "generated request id"},
		{name: "invalid request id", requestIDs: []string{"private-request-id"}},
		{name: "duplicate request id", requestIDs: []string{requestID, requestID}},
		{name: "allowed origin"}, {name: "pending body headers"},
		{name: "null color"}, {name: "empty color"}, {name: "maximum ids"},
		{name: "minimum time"}, {name: "maximum time"}, {name: "reformatted json"}, {name: "escaped name"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 测试 POST 入口接收稳定 JSON，复用当前请求的公共中间件。
			data := createDirectorySuccessFixture(t)
			switch testCase.name {
			case "null color":
				data.Entry.ColorKey = nil
			case "empty color":
				*data.Entry.ColorKey = ""
			case "maximum ids":
				data.Entry.ID, data.Entry.ParentID = "18446744073709551615", "18446744073709551614"
			case "minimum time", "maximum time":
				stamp := time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)
				if testCase.name == "maximum time" {
					stamp = time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
				}
				data.Entry.CreatedAt, data.Entry.ModifiedAt, data.Entry.UpdatedAt = stamp, stamp, stamp
			case "escaped name":
				data.Entry.Name = "Cafe\u0301 <学习>&资料 🌱"
			}
			stableBody, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			if testCase.name == "reformatted json" {
				var fields map[string]any
				if err := json.Unmarshal(stableBody, &fields); err != nil {
					t.Fatal(err)
				}
				stableBody, err = json.MarshalIndent(fields, "", "  ")
				if err != nil {
					t.Fatal(err)
				}
			}
			original := append([]byte(nil), stableBody...)
			var logs bytes.Buffer
			var storedID string
			const origin = "http://127.0.0.1:5173"
			router := NewRouter(logging.New(&logs), []string{origin}, func(context.Context) bool { return true })
			router.POST("/test-create-directory-success", func(requestContext *gin.Context) {
				storedID = requestcontext.RequestID(requestContext.Request.Context())
				if testCase.name == "pending body headers" {
					requestContext.Header("Content-Length", "3")
					requestContext.Header("Content-Encoding", "gzip")
					requestContext.Header("Content-Type", "application/octet-stream")
				}
				handler.CreateDirectorySuccess(requestContext, stableBody)
			}, func(*gin.Context) { t.Error("创建成功交付后进入下游") })
			request := httptest.NewRequest("POST", "/test-create-directory-success?note=private-query", strings.NewReader("private-body"))
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

			// 2. 核对 201、完整快照、精确字段和当前请求 ID。
			if response.Code != 201 || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" ||
				response.Header().Get("Content-Type") != "application/json; charset=utf-8" || response.Header().Get("Content-Length") != strconv.Itoa(response.Body.Len()) ||
				response.Header().Get("Content-Encoding") != "" || len(response.Header().Values("Set-Cookie")) != 0 {
				t.Fatal("创建成功状态或响应头错误")
			}
			var body dto.CreateDirectoryResponse
			if json.Unmarshal(response.Body.Bytes(), &body) != nil || !reflect.DeepEqual(body.Data, *data) || body.RequestID == "" || body.RequestID != storedID || body.RequestID != response.Header().Get("X-Request-ID") {
				t.Fatal("创建 JSON 或请求关联错误")
			}
			parsedID, err := uuid.Parse(body.RequestID)
			if err != nil || parsedID.Version() != 4 || (testCase.expectedID != "" && body.RequestID != testCase.expectedID) {
				t.Fatal("请求 ID 生成或规范化错误")
			}
			var outer map[string]json.RawMessage
			var payload map[string]json.RawMessage
			var entry map[string]json.RawMessage
			if json.Unmarshal(response.Body.Bytes(), &outer) != nil || len(outer) != 2 ||
				json.Unmarshal(outer["data"], &payload) != nil || len(payload) != 1 || json.Unmarshal(payload["entry"], &entry) != nil || len(entry) != 11 {
				t.Fatal("创建响应字段数量错误")
			}
			if data.Entry.ColorKey == nil && string(entry["color_key"]) != "null" {
				t.Fatal("空颜色未明确编码为 null")
			}

			// 3. 核对完成日志、跨域和稳定输入，保护原始请求与展示内容。
			var completed map[string]any
			if json.Unmarshal(logs.Bytes(), &completed) != nil || completed["request_id"] != body.RequestID || completed["status"] != float64(201) ||
				completed["path"] != "/test-create-directory-success" || completed["method"] != "POST" || completed["level"] != "INFO" {
				t.Fatal("创建完成日志错误")
			}
			if testCase.name == "allowed origin" && (response.Header().Get("Access-Control-Allow-Origin") != origin || response.Header().Get("Access-Control-Allow-Credentials") != "true") {
				t.Fatal("创建成功跨域响应头错误")
			}
			for _, marker := range []string{"private-query", "private-body", "private-access", "private-key", "private-refresh", "private-request-id"} {
				if strings.Contains(response.Body.String(), marker) || strings.Contains(logs.String(), marker) {
					t.Fatal("创建正文或日志包含原始请求值")
				}
			}
			if strings.Contains(logs.String(), data.Entry.Name) || !bytes.Equal(stableBody, original) {
				t.Fatal("日志包含展示名称或交付修改了稳定输入")
			}
		})
	}
}

func TestCreateDirectorySuccessFailures(t *testing.T) {
	for _, failure := range []string{
		"nil data", "invalid json", "null", "array", "invalid utf8", "missing size", "missing color",
		"unknown field", "numeric id", "later revision", "invalid time", "invalid color",
		"cancelled", "deadline exceeded", "cancel before write",
	} {
		t.Run(failure, func(t *testing.T) {
			data := createDirectorySuccessFixture(t)
			stableBody, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var callContext context.Context = ctx
			switch failure {
			case "nil data":
				stableBody = nil
			case "invalid json":
				stableBody = []byte("private-invalid-json")
			case "null", "array":
				stableBody = []byte("null")
				if failure == "array" {
					stableBody = []byte("[]")
				}
			case "invalid utf8":
				stableBody = bytes.Replace(stableBody, []byte("学习资料"), []byte{0xff}, 1)
			case "missing size":
				stableBody = bytes.Replace(stableBody, []byte(`"size_bytes":0,`), nil, 1)
			case "missing color":
				stableBody = bytes.Replace(stableBody, []byte(`,"color_key":"green"`), nil, 1)
			case "unknown field":
				stableBody = bytes.Replace(stableBody, []byte(`"id":"42"`), []byte(`"id":"42","private_field":true`), 1)
			case "numeric id":
				stableBody = bytes.Replace(stableBody, []byte(`"id":"42"`), []byte(`"id":42`), 1)
			case "later revision":
				stableBody = bytes.Replace(stableBody, []byte(`"revision":"1"`), []byte(`"revision":"2"`), 1)
			case "invalid time":
				stableBody = bytes.Replace(stableBody, []byte(`"created_at":"2026-11-16T01:30:00.123456Z"`), []byte(`"created_at":"private-time"`), 1)
			case "invalid color":
				stableBody = bytes.Replace(stableBody, []byte(`"color_key":"green"`), []byte(`"color_key":"private-color-超长"`), 1)
			case "cancelled":
				cancel()
			case "deadline exceeded":
				expired, release := context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer release()
				callContext = expired
			}
			var logs bytes.Buffer
			router := NewRouter(logging.New(&logs), nil, func(context.Context) bool { return true })
			router.POST("/test-create-directory-success", func(requestContext *gin.Context) {
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
				requestContext.Header("Content-Type", "application/octet-stream")
				if failure == "cancel before write" {
					lateContext, cancelLate := context.WithCancel(requestContext.Request.Context())
					defer cancelLate()
					requestContext.Request = requestContext.Request.WithContext(&cancelBeforeRefreshWriteContext{Context: lateContext, cancel: cancelLate})
				}
				handler.CreateDirectorySuccess(requestContext, stableBody)
			}, func(*gin.Context) { t.Error("创建失败交付后进入下游") })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest("POST", "/test-create-directory-success?note=private-query", nil).WithContext(callContext))
			var body dto.ErrorResponse
			if json.Unmarshal(response.Body.Bytes(), &body) != nil || response.Code != 500 || body.Error.Code != "INTERNAL_ERROR" || body.Error.Message != "服务内部错误，请稍后重试" || body.Error.Details != nil ||
				body.RequestID == "" || body.RequestID != response.Header().Get("X-Request-ID") || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" ||
				response.Header().Get("Content-Type") != "application/json; charset=utf-8" || response.Header().Get("Content-Encoding") != "" || response.Header().Get("Content-Length") != "" || len(response.Header().Values("Set-Cookie")) != 0 {
				t.Fatal("创建失败响应或头信息错误")
			}
			var completed map[string]any
			if json.Unmarshal(logs.Bytes(), &completed) != nil || completed["status"] != float64(500) || completed["level"] != "ERROR" || completed["request_id"] != body.RequestID {
				t.Fatal("创建失败完成日志错误")
			}
			for _, marker := range []string{"private-query", "private-invalid-json", "private_field", "private-time", "private-color", "学习资料"} {
				if strings.Contains(response.Body.String(), marker) || strings.Contains(logs.String(), marker) {
					t.Fatal("创建失败正文或日志包含内部数据")
				}
			}
		})
	}
}

func TestCreateDirectorySuccessCommittedResponse(t *testing.T) {
	for _, mode := range []string{"body written", "headers flushed"} {
		t.Run(mode, func(t *testing.T) {
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.POST("/test-create-directory-success", func(requestContext *gin.Context) {
				if mode == "body written" {
					requestContext.String(202, "already-written")
				} else {
					requestContext.Status(202)
					requestContext.Writer.WriteHeaderNow()
				}
				cancelled, cancel := context.WithCancel(requestContext.Request.Context())
				cancel()
				requestContext.Request = requestContext.Request.WithContext(cancelled)
				handler.CreateDirectorySuccess(requestContext, nil)
			}, func(*gin.Context) { t.Error("已提交响应后进入下游") })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest("POST", "/test-create-directory-success", nil))
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

func TestCreateDirectorySuccessHTTP(t *testing.T) {
	for _, mode := range []string{"saved snapshot", "null color", "invalid data"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 使用空闲端口，以实际 POST 验证稳定结果的成功与错误传输。
			data := createDirectorySuccessFixture(t)
			if mode == "null color" {
				data.Entry.ColorKey = nil
			}
			stableBody, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			status := 201
			if mode == "invalid data" {
				stableBody, status = []byte(`{"entry":null}`), 500
			}
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.POST("/test-create-directory-success", func(requestContext *gin.Context) {
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
				handler.CreateDirectorySuccess(requestContext, stableBody)
			})
			server := httptest.NewServer(router)
			defer server.Close()
			client := server.Client()
			client.Timeout = 2 * time.Second
			var previousID string
			for _, requestID := range []string{"28EE758A-0727-42DE-AE7D-3BB33155E148", "28EE758A-0727-42DE-AE7D-3BB33155E149"} {
				request, err := http.NewRequest("POST", server.URL+"/test-create-directory-success", nil)
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("X-Request-ID", requestID)
				response, err := client.Do(request)
				if err != nil {
					t.Fatal("真实创建 HTTP 请求失败", err)
				}
				encoded, readErr := io.ReadAll(response.Body)
				closeErr := response.Body.Close()
				if readErr != nil || closeErr != nil || response.StatusCode != status || response.ContentLength != int64(len(encoded)) ||
					response.Header.Get("Content-Type") != "application/json; charset=utf-8" || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Pragma") != "no-cache" ||
					response.Header.Get("Content-Encoding") != "" || len(response.Cookies()) != 0 {
					t.Fatal("真实创建状态、正文或响应头错误", readErr, closeErr)
				}

				// 2. 同一稳定数据可关联各次请求 ID，核对完整快照或统一错误。
				currentID := response.Header.Get("X-Request-ID")
				if currentID != strings.ToLower(requestID) || currentID == previousID {
					t.Fatal("稳定数据复用时未关联当前请求 ID")
				}
				previousID = currentID
				if status == 201 {
					var body dto.CreateDirectoryResponse
					if json.Unmarshal(encoded, &body) != nil || !reflect.DeepEqual(body.Data, *data) || body.RequestID != currentID {
						t.Fatal("真实创建成功 JSON 错误")
					}
				} else {
					var body dto.ErrorResponse
					if json.Unmarshal(encoded, &body) != nil || body.Error.Code != "INTERNAL_ERROR" || body.Error.Details != nil || body.RequestID != currentID {
						t.Fatal("真实创建错误 JSON 错误")
					}
				}
			}
		})
	}
}
