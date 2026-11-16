package http

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
)

func TestSizeEntryPageQueryHTTPFlow(t *testing.T) {
	for _, testCase := range []struct {
		name, raw, field, search, kind, cursor, scope string
		size                                          int
	}{
		{name: "starred default query", raw: "scope=starred", scope: "starred", size: 50},
		{name: "starred encoded scope", raw: "%73cope=%73tarred&limit=1", scope: "starred", size: 1},
		{name: "starred filters", raw: "scope=starred&q=" + url.QueryEscape(" private-学习%_+ ") + "&kind=document&limit=2", scope: "starred", search: " private-学习%_+ ", kind: "document", size: 2},
		{name: "starred next cursor", raw: "scope=starred&cursor=private-cursor&limit=100", scope: "starred", cursor: "private-cursor", size: 100},
		{name: "long accepted cursor", raw: "cursor=" + strings.Repeat("a", 355), cursor: strings.Repeat("a", 355), size: 50},
		{name: "maximum accepted cursor", raw: "cursor=" + strings.Repeat("a", 1024), cursor: strings.Repeat("a", 1024), size: 50},
		{name: "defaults", size: 50},
		{name: "explicit mode", raw: "scope=all&sort=size_desc&q=&kind=&cursor=", size: 50},
		{name: "search and category", raw: "q=" + url.QueryEscape(" private-学习%_+ ") + "&kind=document&limit=2", search: " private-学习%_+ ", kind: "document", size: 2},
		{name: "next page", raw: "cursor=private-cursor&limit=100", cursor: "private-cursor", size: 100},
		{name: "encoded keys", raw: "%71=%E5%AD%A6%E4%B9%A0&%6Bind=folder&%6Cimit=%31", search: "学习", kind: "folder", size: 1},
		{name: "ignored identity", raw: "user_id=99&parent_id=private-parent&note=private-query", size: 50},
		{name: "unsupported scope", raw: "scope=recent", field: "scope"},
		{name: "empty scope", raw: "scope=", field: "scope"},
		{name: "duplicate scope", raw: "scope=all&%73cope=all", field: "scope"},
		{name: "unsupported sort", raw: "sort=modified_desc", field: "sort"},
		{name: "duplicate sort", raw: "sort=size_desc&sort=size_desc", field: "sort"},
		{name: "duplicate search", raw: "q=private-one&q=private-two", field: "q"},
		{name: "invalid utf8 search", raw: "q=%FF", field: "q"},
		{name: "invalid kind", raw: "kind=private-kind", field: "kind"},
		{name: "duplicate kind", raw: "kind=&kind=", field: "kind"},
		{name: "duplicate cursor", raw: "cursor=private-one&cursor=private-two", field: "cursor"},
		{name: "long cursor", raw: "cursor=" + strings.Repeat("a", 1025), field: "cursor"},
		{name: "empty limit", raw: "limit=", field: "limit"},
		{name: "zero limit", raw: "limit=0", field: "limit"},
		{name: "duplicate limit", raw: "limit=1&limit=2", field: "limit"},
		{name: "malformed extra", raw: "note=private-%ZZ", field: "query"},
		{name: "semicolon query", raw: "scope=all;limit=1", field: "query"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 测试入口解析完整 RawQuery，按结果交付成功或固定字段错误。
			var logs bytes.Buffer
			var accepted *dto.SizeEntryPageQuery
			downstream := 0
			router := NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
			router.GET("/test-size-entry-query", func(requestContext *gin.Context) {
				input, fieldError := dto.ParseSizeEntryPageQuery(requestContext.Request.URL.RawQuery)
				if fieldError != nil {
					handler.InvalidArgument(requestContext, fieldError)
					return
				}
				accepted = input
				handler.SizeEntryPageSuccess(requestContext, sizeEntryPageSuccessFixture())
			}, func(*gin.Context) { downstream++ })
			request := httptest.NewRequest("GET", "/test-size-entry-query", nil)
			request.URL.RawQuery = testCase.raw
			request.RequestURI = request.URL.RequestURI()
			request.Header.Set("Origin", "http://127.0.0.1:5173")
			request.Header.Set("X-Request-ID", "28EE758A-0727-42DE-AE7D-3BB33155E148")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			// 2. 核对条件、请求 ID、缓存、CORS 和下游中止。
			if response.Header().Get("X-Request-ID") != "28ee758a-0727-42de-ae7d-3bb33155e148" || response.Header().Get("Access-Control-Allow-Origin") != "http://127.0.0.1:5173" || response.Header().Get("Cache-Control") != "no-store" || downstream != 0 {
				t.Fatal("请求 ID、CORS、缓存或下游中止错误")
			}
			if testCase.field == "" {
				var body dto.SizeEntryPageResponse
				expected := domain.GlobalEntryQuery{Scope: "all", Sort: "size_desc", Search: testCase.search, Kind: testCase.kind, PageSize: testCase.size}
				if testCase.scope != "" {
					expected.Scope = testCase.scope
				}
				if response.Code != 200 || accepted == nil || accepted.Query != expected || accepted.Cursor != testCase.cursor || json.Unmarshal(response.Body.Bytes(), &body) != nil || len(body.Data.Items) != 1 || body.RequestID != response.Header().Get("X-Request-ID") {
					t.Fatal("解析或大小成功交付衔接错误")
				}
			} else {
				var body dto.ErrorResponse
				if response.Code != 400 || accepted != nil || json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Error.Code != "INVALID_ARGUMENT" || body.RequestID != response.Header().Get("X-Request-ID") || len(body.Error.Details) != 1 || body.Error.Details["field"] != testCase.field {
					t.Fatal("统一字段错误或失败结果错误")
				}
			}

			// 3. 错误正文和完成日志保持固定公开字段。
			if strings.Contains(response.Body.String(), "private-") || strings.Contains(logs.String(), "private-") || strings.Contains(logs.String(), "学习") {
				t.Fatal("正文或日志包含原始查询值")
			}
		})
	}
}
