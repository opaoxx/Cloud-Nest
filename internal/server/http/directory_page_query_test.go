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

func TestDirectoryPageQueryHTTPFlow(t *testing.T) {
	for _, testCase := range []struct {
		name, raw, field, cursor string
		parentID                 uint64
		size                     int
	}{
		{name: "defaults", raw: "parent_id=12", parentID: 12, size: 50},
		{name: "empty first cursor", raw: "parent_id=12&cursor=", parentID: 12, size: 50},
		{name: "next page", raw: "parent_id=12&cursor=private-opaque-cursor&limit=2", parentID: 12, cursor: "private-opaque-cursor", size: 2},
		{name: "encoded fields", raw: "parent%5Fid=%31%32&cursor=A%2EB&limit=%32", parentID: 12, cursor: "A.B", size: 2},
		{name: "full unsigned parent", raw: "parent_id=18446744073709551615&limit=100", parentID: ^uint64(0), size: 100},
		{name: "explicit supported mode", raw: "parent_id=12&scope=directory&sort=modified_desc&q=&kind=", parentID: 12, size: 50},
		{name: "extra identity ignored", raw: "parent_id=12&user_id=99&note=private-query", parentID: 12, size: 50},
		{name: "missing parent", field: "parent_id"},
		{name: "zero parent", raw: "parent_id=0", field: "parent_id"},
		{name: "duplicate parent", raw: "parent_id=12&parent%5Fid=12", field: "parent_id"},
		{name: "overflow parent", raw: "parent_id=18446744073709551616", field: "parent_id"},
		{name: "duplicate cursor", raw: "parent_id=12&cursor=private-one&cursor=private-two", field: "cursor"},
		{name: "long cursor", raw: "parent_id=12&cursor=" + strings.Repeat("a", 1025), field: "cursor"},
		{name: "empty limit", raw: "parent_id=12&limit=", field: "limit"},
		{name: "zero limit", raw: "parent_id=12&limit=0", field: "limit"},
		{name: "duplicate limit", raw: "parent_id=12&limit=1&limit=2", field: "limit"},
		{name: "scope mismatch", raw: "parent_id=12&scope=private-scope", field: "scope"},
		{name: "sort mismatch", raw: "parent_id=12&sort=private-sort", field: "sort"},
		{name: "search mismatch", raw: "parent_id=12&q=private-search", field: "q"},
		{name: "kind mismatch", raw: "parent_id=12&kind=private-kind", field: "kind"},
		{name: "bad extra encoding", raw: "parent_id=12&note=private-%ZZ", field: "query"},
		{name: "semicolon query", raw: "parent_id=12;limit=1", field: "query"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 测试入口解析完整 RawQuery，成功后复用已有目录页交付。
			var logs bytes.Buffer
			var accepted *dto.DirectoryPageQuery
			downstream := 0
			router := NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
			router.GET("/test-directory-query", func(requestContext *gin.Context) {
				input, fieldError := dto.ParseDirectoryPageQuery(requestContext.Request.URL.RawQuery)
				if fieldError != nil {
					handler.InvalidArgument(requestContext, fieldError)
					return
				}
				accepted = input
				handler.DirectoryPageSuccess(requestContext, directoryPageSuccessFixture())
			}, func(*gin.Context) { downstream++ })
			request := httptest.NewRequest("GET", "/test-directory-query", nil)
			request.URL.RawQuery = testCase.raw
			request.RequestURI = request.URL.RequestURI()
			request.Header.Set("Origin", "http://127.0.0.1:5173")
			request.Header.Set("X-Request-ID", "28EE758A-0727-42DE-AE7D-3BB33155E148")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			// 2. 检查类型化参数、统一字段错误、请求 ID 和结束处理。
			if response.Header().Get("X-Request-ID") != "28ee758a-0727-42de-ae7d-3bb33155e148" || response.Header().Get("Access-Control-Allow-Origin") != "http://127.0.0.1:5173" || downstream != 0 {
				t.Fatal("请求 ID、CORS 或下游中止错误")
			}
			if testCase.field == "" {
				var body dto.DirectoryPageResponse
				if response.Code != 200 || accepted == nil || accepted.ParentID != testCase.parentID || accepted.Cursor != testCase.cursor || accepted.PageSize != testCase.size || json.Unmarshal(response.Body.Bytes(), &body) != nil || len(body.Data.Items) != 1 || body.RequestID != response.Header().Get("X-Request-ID") {
					t.Fatal("解析及成功交付衔接错误")
				}
			} else {
				var body dto.ErrorResponse
				if response.Code != 400 || accepted != nil || json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Error.Code != "INVALID_ARGUMENT" || body.RequestID != response.Header().Get("X-Request-ID") || len(body.Error.Details) != 1 || body.Error.Details["field"] != testCase.field || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("统一字段错误错误或使用了部分参数")
				}
			}

			// 3. 日志与错误正文保持固定字段及公开提示。
			if strings.Contains(response.Body.String(), "private-") || strings.Contains(logs.String(), "private-") {
				t.Fatal("正文或日志包含原始查询值")
			}
		})
	}
}
