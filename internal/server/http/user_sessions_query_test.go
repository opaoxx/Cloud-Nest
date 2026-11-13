package http

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
)

func TestUserSessionListQueryHTTPFlow(t *testing.T) {
	for _, testCase := range []struct {
		name, raw, field string
		beforeID         uint64
		pageSize         int
	}{
		{name: "defaults", pageSize: 50},
		{name: "explicit first page", raw: "before_id=0&limit=50", pageSize: 50},
		{name: "next page", raw: "before_id=13&limit=2", beforeID: 13, pageSize: 2},
		{name: "full unsigned boundary", raw: "before_id=18446744073709551615&limit=100", beforeID: ^uint64(0), pageSize: 100},
		{name: "ignored extra identity", raw: "user_id=99&session_id=private-session", pageSize: 50},
		{name: "empty limit", raw: "limit=", field: "limit"},
		{name: "invalid limit", raw: "limit=private-limit", field: "limit"},
		{name: "duplicate limit", raw: "limit=1&limit=2", field: "limit"},
		{name: "zero limit", raw: "limit=0", field: "limit"},
		{name: "oversized limit", raw: "limit=101", field: "limit"},
		{name: "empty boundary", raw: "before_id=", field: "before_id"},
		{name: "invalid boundary", raw: "before_id=private-boundary", field: "before_id"},
		{name: "overflow boundary", raw: "before_id=18446744073709551616", field: "before_id"},
		{name: "duplicate boundary", raw: "before_id=13&before_id=11", field: "before_id"},
		{name: "malformed query", raw: "limit=1&note=private-%ZZ", field: "query"},
		{name: "semicolon separator", raw: "before_id=13;limit=2", field: "query"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 测试入口直接解析 RawQuery，避免丢失查询字符串解析错误。
			var logs bytes.Buffer
			router := NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
			var accepted *dto.UserSessionListQuery
			router.GET("/test-session-query", func(requestContext *gin.Context) {
				input, fieldError := dto.ParseUserSessionListQuery(requestContext.Request.URL.RawQuery)
				if fieldError != nil {
					handler.InvalidArgument(requestContext, fieldError)
					return
				}
				accepted = input
				requestContext.Status(204)
			})
			request := httptest.NewRequest("GET", "/test-session-query", nil)
			request.URL.RawQuery = testCase.raw
			request.RequestURI = request.URL.RequestURI()
			request.Header.Set("Origin", "http://127.0.0.1:5173")
			request.Header.Set("X-Request-ID", "28EE758A-0727-42DE-AE7D-3BB33155E148")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			// 2. 检查已解析参数、统一字段错误及请求标识。
			if response.Header().Get("X-Request-ID") != "28ee758a-0727-42de-ae7d-3bb33155e148" || response.Header().Get("Access-Control-Allow-Origin") != "http://127.0.0.1:5173" {
				t.Fatal("请求 ID 或 CORS 头错误")
			}
			if testCase.field == "" {
				if response.Code != 204 || accepted == nil || accepted.BeforeSessionID != testCase.beforeID || accepted.PageSize != testCase.pageSize || response.Body.Len() != 0 {
					t.Fatal("成功解析后的类型化参数错误")
				}
			} else {
				var body dto.ErrorResponse
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != 400 || accepted != nil || body.Error.Code != "INVALID_ARGUMENT" ||
					body.RequestID != response.Header().Get("X-Request-ID") || len(body.Error.Details) != 1 || body.Error.Details["field"] != testCase.field || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("统一字段错误或处理顺序错误", err)
				}
			}

			// 3. 核对响应和完成日志中的原始查询值保护。
			if strings.Contains(response.Body.String(), "private-") || strings.Contains(logs.String(), "private-") {
				t.Fatal("响应或日志包含原始查询值")
			}
		})
	}
}
