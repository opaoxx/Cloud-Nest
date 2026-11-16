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

func setStarredSuccessFixture() *domain.EntryPreference {
	stamp := time.Date(2026, 11, 16, 10, 30, 0, 123456789, time.FixedZone("UTC+8", 8*60*60))
	return &domain.EntryPreference{UserID: 7, EntryID: 13, StarredAt: &stamp, LastOpenedAt: &stamp}
}

func TestSetStarredSuccessDelivery(t *testing.T) {
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	for _, mode := range []string{"existing request id", "uppercase request id", "generated request id", "invalid request id", "duplicate request id", "allowed origin", "pending body headers", "unstarred", "recent access only", "maximum identifiers", "minimum date", "maximum date", "submicrosecond time"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 准备已确认偏好及测试入口，公共中间件提供当前请求 ID。
			result := setStarredSuccessFixture()
			requestIDs, expectedID := []string{requestID}, requestID
			switch mode {
			case "uppercase request id":
				requestIDs = []string{strings.ToUpper(requestID)}
			case "generated request id":
				requestIDs, expectedID = nil, ""
			case "invalid request id":
				requestIDs, expectedID = []string{"private-request-id"}, ""
			case "duplicate request id":
				requestIDs, expectedID = []string{requestID, requestID}, ""
			case "unstarred":
				result.StarredAt, result.LastOpenedAt = nil, nil
			case "recent access only":
				result.StarredAt = nil
			case "maximum identifiers":
				result.UserID, result.EntryID = ^uint64(0), ^uint64(0)
			case "minimum date":
				*result.StarredAt = time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)
			case "maximum date":
				*result.StarredAt = time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
			case "submicrosecond time":
				*result.StarredAt = time.Date(2026, 11, 16, 2, 30, 0, 999, time.UTC)
			}
			before, marshalError := json.Marshal(result)
			if marshalError != nil {
				t.Fatal(marshalError)
			}
			var logs bytes.Buffer
			var storedID string
			origin := "http://127.0.0.1:5173"
			router := NewRouter(logging.New(&logs), []string{origin}, func(context.Context) bool { return true })
			router.PUT("/test-set-starred-success", func(requestContext *gin.Context) {
				storedID = requestcontext.RequestID(requestContext.Request.Context())
				if mode == "pending body headers" {
					requestContext.Header("Content-Length", "3")
					requestContext.Header("Content-Encoding", "gzip")
					requestContext.Header("Content-Type", "application/octet-stream")
				}
				handler.SetStarredSuccess(requestContext, result)
			}, func(*gin.Context) { t.Error("成功交付后进入下游") })
			request := httptest.NewRequest("PUT", "/test-set-starred-success?note=private-query", nil)
			for _, value := range requestIDs {
				request.Header.Add("X-Request-ID", value)
			}
			if mode == "allowed origin" {
				request.Header.Set("Origin", origin)
			}
			request.Header.Set("Authorization", "Bearer private-access")
			request.AddCookie(&http.Cookie{Name: handler.RefreshCookieName, Value: "private-refresh"})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			// 2. 核对状态、缓存头、正文头及精确公开字段。
			if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" ||
				response.Header().Get("Content-Type") != "application/json; charset=utf-8" || response.Header().Get("Content-Length") != strconv.Itoa(response.Body.Len()) ||
				response.Header().Get("Content-Encoding") != "" || len(response.Header().Values("Set-Cookie")) != 0 {
				t.Fatal("成功状态或响应头错误")
			}
			var body dto.SetStarredResponse
			if json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Data.Starred != (result.StarredAt != nil) || body.RequestID == "" || body.RequestID != storedID || body.RequestID != response.Header().Get("X-Request-ID") {
				t.Fatal("成功 JSON 或请求 ID 错误")
			}
			parsedID, err := uuid.Parse(body.RequestID)
			if err != nil || parsedID.Version() != 4 || (expectedID != "" && body.RequestID != expectedID) {
				t.Fatal("请求 ID 生成、保留或规范化错误")
			}
			var fields map[string]json.RawMessage
			var payload map[string]any
			if json.Unmarshal(response.Body.Bytes(), &fields) != nil || len(fields) != 2 || json.Unmarshal(fields["data"], &payload) != nil || len(payload) != 2 || payload["starred"] != body.Data.Starred {
				t.Fatal("公开响应字段错误")
			}
			if result.StarredAt == nil {
				if body.Data.StarredAt != nil || payload["starred_at"] != nil {
					t.Fatal("取消收藏时间未交付 null")
				}
			} else if body.Data.StarredAt == nil || !body.Data.StarredAt.Equal(result.StarredAt.UTC().Truncate(time.Microsecond)) || payload["starred_at"] != result.StarredAt.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano) {
				t.Fatal("收藏时间或精度错误")
			}

			// 3. 关联完成日志，核对输入原值和请求数据保护。
			var completed map[string]any
			if json.Unmarshal(logs.Bytes(), &completed) != nil || completed["request_id"] != body.RequestID || completed["status"] != float64(200) || completed["method"] != "PUT" || completed["path"] != "/test-set-starred-success" {
				t.Fatal("完成日志的请求 ID、状态或路径错误")
			}
			if mode == "allowed origin" && response.Header().Get("Access-Control-Allow-Origin") != origin {
				t.Fatal("成功响应缺少 CORS 头")
			}
			for _, marker := range []string{"private-query", "private-access", "private-refresh", "private-request-id", "user_id", "entry_id", "last_opened_at"} {
				if strings.Contains(response.Body.String(), marker) || strings.Contains(logs.String(), marker) {
					t.Fatal("正文或日志包含内部数据")
				}
			}
			after, err := json.Marshal(result)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("交付修改了服务结果", err)
			}
		})
	}
}

func TestSetStarredSuccessFailures(t *testing.T) {
	for _, mode := range []string{"nil result", "zero user", "zero entry", "zero time", "early time", "late time", "UTC year below range", "UTC year above range", "canceled", "deadline exceeded", "cancel before write"} {
		t.Run(mode, func(t *testing.T) {
			result := setStarredSuccessFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var callContext context.Context = ctx
			switch mode {
			case "nil result":
				result = nil
			case "zero user":
				result.UserID = 0
			case "zero entry":
				result.EntryID = 0
			case "zero time":
				*result.StarredAt = time.Time{}
			case "early time":
				*result.StarredAt = time.Date(999, 1, 1, 0, 0, 0, 0, time.UTC)
			case "late time":
				*result.StarredAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
			case "UTC year below range":
				*result.StarredAt = time.Date(1000, 1, 1, 0, 30, 0, 0, time.FixedZone("UTC+1", 3600))
			case "UTC year above range":
				*result.StarredAt = time.Date(9999, 12, 31, 23, 30, 0, 0, time.FixedZone("UTC-1", -3600))
			case "canceled":
				cancel()
			case "deadline exceeded":
				expired, release := context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer release()
				callContext = expired
			}
			var logs bytes.Buffer
			router := NewRouter(logging.New(&logs), nil, func(context.Context) bool { return true })
			router.PUT("/test-set-starred-success", func(requestContext *gin.Context) {
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
				requestContext.Header("Content-Type", "application/octet-stream")
				if mode == "cancel before write" {
					lateContext, cancelLate := context.WithCancel(requestContext.Request.Context())
					defer cancelLate()
					requestContext.Request = requestContext.Request.WithContext(&cancelBeforeRefreshWriteContext{Context: lateContext, cancel: cancelLate})
				}
				handler.SetStarredSuccess(requestContext, result)
			}, func(*gin.Context) { t.Error("失败交付后进入下游") })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest("PUT", "/test-set-starred-success?note=private-query", nil).WithContext(callContext))
			var body dto.ErrorResponse
			if json.Unmarshal(response.Body.Bytes(), &body) != nil || response.Code != 500 || body.Error.Code != "INTERNAL_ERROR" || body.Error.Message != "服务内部错误，请稍后重试" || body.Error.Details != nil ||
				body.RequestID == "" || body.RequestID != response.Header().Get("X-Request-ID") || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" ||
				response.Header().Get("Content-Type") != "application/json; charset=utf-8" || response.Header().Get("Content-Encoding") != "" || response.Header().Get("Content-Length") != "" || len(response.Header().Values("Set-Cookie")) != 0 {
				t.Fatal("失败交付的错误结构或响应头错误")
			}
			var completed map[string]any
			if json.Unmarshal(logs.Bytes(), &completed) != nil || completed["status"] != float64(500) || completed["level"] != "ERROR" || completed["request_id"] != body.RequestID {
				t.Fatal("失败完成日志错误")
			}
			if strings.Contains(logs.String(), "private-query") || strings.Contains(response.Body.String(), "starred") {
				t.Fatal("失败交付包含原始查询或部分成功结果")
			}
		})
	}
}

func TestSetStarredSuccessCommittedResponse(t *testing.T) {
	for _, mode := range []string{"body written", "headers flushed"} {
		t.Run(mode, func(t *testing.T) {
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.PUT("/test-set-starred-success", func(requestContext *gin.Context) {
				if mode == "body written" {
					requestContext.String(202, "already-written")
				} else {
					requestContext.Status(202)
					requestContext.Writer.WriteHeaderNow()
				}
				canceled, cancel := context.WithCancel(requestContext.Request.Context())
				cancel()
				requestContext.Request = requestContext.Request.WithContext(canceled)
				handler.SetStarredSuccess(requestContext, nil)
			}, func(*gin.Context) { t.Error("已提交响应后进入下游") })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest("PUT", "/test-set-starred-success", nil))
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

func TestSetStarredSuccessHTTP(t *testing.T) {
	for _, mode := range []string{"starred", "unstarred", "invalid data"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 使用空闲端口，以真实 PUT 验证成功、空值和异常传输。
			result, expectedStatus := setStarredSuccessFixture(), 200
			if mode == "unstarred" {
				result.StarredAt = nil
			}
			if mode == "invalid data" {
				result, expectedStatus = nil, 500
			}
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.PUT("/test-set-starred-success", func(requestContext *gin.Context) {
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
				handler.SetStarredSuccess(requestContext, result)
			})
			server := httptest.NewServer(router)
			defer server.Close()
			client := server.Client()
			client.Timeout = 2 * time.Second
			var previousID string
			var previousData dto.SetStarredData
			for _, requestID := range []string{"28EE758A-0727-42DE-AE7D-3BB33155E148", "28EE758A-0727-42DE-AE7D-3BB33155E149"} {
				request, err := http.NewRequest("PUT", server.URL+"/test-set-starred-success", nil)
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("X-Request-ID", requestID)
				response, err := client.Do(request)
				if err != nil {
					t.Fatal("真实 PUT 请求失败", err)
				}
				encoded, readError := io.ReadAll(response.Body)
				closeError := response.Body.Close()
				if readError != nil || closeError != nil || response.StatusCode != expectedStatus || response.ContentLength != int64(len(encoded)) || response.Header.Get("Content-Type") != "application/json; charset=utf-8" ||
					response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Pragma") != "no-cache" || response.Header.Get("Content-Encoding") != "" || len(response.Cookies()) != 0 {
					t.Fatal("真实响应状态、正文或头信息错误", readError, closeError)
				}

				// 2. 同一已提交偏好在不同请求中关联各自 ID。
				currentID := response.Header.Get("X-Request-ID")
				if currentID != strings.ToLower(requestID) || currentID == previousID {
					t.Fatal("响应未关联当前请求 ID")
				}
				if expectedStatus == 200 {
					var body dto.SetStarredResponse
					if json.Unmarshal(encoded, &body) != nil || body.RequestID != currentID || body.Data.Starred != (mode == "starred") || (body.Data.StarredAt != nil) != body.Data.Starred {
						t.Fatal("真实成功 JSON 错误")
					}
					if body.Data.Starred && !body.Data.StarredAt.Equal(result.StarredAt.UTC().Truncate(time.Microsecond)) {
						t.Fatal("真实收藏时间错误")
					}
					if previousID != "" && !reflect.DeepEqual(previousData, body.Data) {
						t.Fatal("重复交付的偏好事实错误")
					}
					previousData = body.Data
				} else {
					var body dto.ErrorResponse
					if json.Unmarshal(encoded, &body) != nil || body.Error.Code != "INTERNAL_ERROR" || body.Error.Details != nil || body.RequestID != currentID {
						t.Fatal("真实错误 JSON 错误")
					}
				}
				previousID = currentID
			}
		})
	}
}
