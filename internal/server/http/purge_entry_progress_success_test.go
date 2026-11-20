package http

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
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
)

const purgeProgressSuccessTestPath = "/test-purge-entry-progress-success"

func TestPurgeEntryProgressSuccessHTTP(t *testing.T) {
	const suppliedID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	for _, scenario := range []struct {
		result       domain.PurgeProgress
		requestID    string
		expectedData string
	}{
		{domain.PurgeProgress{EntryID: math.MaxUint64, Status: "PURGING", RemainingCount: 2}, strings.ToUpper(suppliedID),
			`{"entry_id":"18446744073709551615","status":"PURGING","remaining_count":2}`},
		{domain.PurgeProgress{EntryID: 23, Status: "PURGED", RemainingCount: 0}, "",
			`{"entry_id":"23","status":"PURGED","remaining_count":0}`},
	} {
		t.Run(scenario.result.Status, func(t *testing.T) {
			// 1. 通过测试路由发出真实 HTTP 请求，预置旧的正文响应头。
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.GET(purgeProgressSuccessTestPath, func(requestContext *gin.Context) {
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
				requestContext.Header("Content-Type", "application/octet-stream")
				handler.PurgeEntryProgressSuccess(requestContext, &scenario.result)
			}, func(*gin.Context) { t.Error("交付进度响应后继续执行了下游") })
			server := httptest.NewServer(router)
			defer server.Close()
			client := server.Client()
			client.Timeout = 3 * time.Second
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+purgeProgressSuccessTestPath, nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Accept-Encoding", "identity")
			if scenario.requestID != "" {
				request.Header.Set("X-Request-ID", scenario.requestID)
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, readErr := io.ReadAll(response.Body)
			closeErr := response.Body.Close()

			// 2. 核对公开正文、当次 ID、缓存头和实际传输长度。
			headers := response.Header
			if readErr != nil || closeErr != nil || response.StatusCode != http.StatusOK ||
				response.ContentLength != int64(len(body)) || headers.Get("Content-Encoding") != "" ||
				headers.Get("Content-Type") != "application/json; charset=utf-8" ||
				headers.Get("Cache-Control") != "no-store" || headers.Get("Pragma") != "no-cache" {
				t.Fatalf("进度响应不符，状态 %d，响应头 %v，读取或关闭错误 %v / %v", response.StatusCode, headers, readErr, closeErr)
			}
			requestID := headers.Get("X-Request-ID")
			parsedID, err := uuid.Parse(requestID)
			if err != nil || parsedID.Version() != 4 || parsedID.Variant() != uuid.RFC4122 ||
				(requestID == suppliedID) != (scenario.requestID != "") {
				t.Fatal("请求 ID 未按本次请求生成或规范化", requestID, err)
			}
			if want := `{"data":` + scenario.expectedData + `,"request_id":"` + requestID + `"}`; string(body) != want {
				t.Fatalf("实际进度正文 %s，期望 %s", body, want)
			}
		})
	}
}

func TestPurgeEntryProgressSuccessFailures(t *testing.T) {
	for _, scenario := range []string{"nil result", "invalid count", "canceled on entry", "canceled before write"} {
		t.Run(scenario, func(t *testing.T) {
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.GET(purgeProgressSuccessTestPath, func(requestContext *gin.Context) {
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
				ctx, cancel := context.WithCancel(requestContext.Request.Context())
				defer cancel()
				result := &domain.PurgeProgress{EntryID: 23, Status: "PURGED", RemainingCount: 0}
				switch scenario {
				case "nil result":
					result = nil
				case "invalid count":
					result.RemainingCount = 1
				case "canceled on entry":
					cancel()
					requestContext.Request = requestContext.Request.WithContext(ctx)
				case "canceled before write":
					requestContext.Request = requestContext.Request.WithContext(&cancelBeforeRefreshWriteContext{Context: ctx, cancel: cancel})
				}
				handler.PurgeEntryProgressSuccess(requestContext, result)
			}, func(*gin.Context) { t.Error("交付错误后继续执行了下游") })

			// 1. 异常时只写统一错误响应，并清理原先的正文头。
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, purgeProgressSuccessTestPath, nil))
			var failure dto.ErrorResponse
			headers := response.Header()
			contentLength := headers.Get("Content-Length")
			if response.Code != http.StatusInternalServerError || json.Unmarshal(response.Body.Bytes(), &failure) != nil ||
				failure.Error.Code != "INTERNAL_ERROR" || failure.Error.Details != nil ||
				failure.RequestID == "" || failure.RequestID != headers.Get("X-Request-ID") ||
				headers.Get("Cache-Control") != "no-store" || headers.Get("Pragma") != "no-cache" ||
				headers.Get("Content-Type") != "application/json; charset=utf-8" || headers.Get("Content-Encoding") != "" ||
				(contentLength != "" && contentLength != strconv.Itoa(response.Body.Len())) ||
				strings.Contains(response.Body.String(), `"data"`) {
				t.Fatalf("进度错误响应不符，状态 %d，响应头 %v，正文 %s", response.Code, headers, response.Body.String())
			}
		})
	}
}
