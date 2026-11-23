package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

func TestReadPullSyncChangesRequest(t *testing.T) {
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	const fixtureToken = "SyntheticToken-a_1.b_2"
	maxToken := strings.Repeat("a", domain.MaxSyncSnapshotTokenLength-2) + ".b"
	for _, testCase := range []struct {
		name, body, contentType string
		want                    dto.PullSyncChangesInput
		status                  int
		code, field             string
	}{
		{name: "baseline defaults to fifty", body: `{"baseline_token":"` + fixtureToken + `"}`,
			want: dto.PullSyncChangesInput{BaselineToken: fixtureToken, PageSize: 50}},
		{name: "escaped key and value", body: `{"curs\u006fr":"SyntheticToken-a_1\u002eb_2","page_size":1}`,
			want: dto.PullSyncChangesInput{Cursor: fixtureToken, PageSize: 1}, contentType: "application/json; charset=UTF-8"},
		{name: "maximum cursor and page size", body: `{"cursor":"` + maxToken + `","page_size":100}`,
			want: dto.PullSyncChangesInput{Cursor: maxToken, PageSize: 100}},
		{name: "missing both credentials", body: "{}"},
		{name: "page size alone", body: `{"page_size":10}`},
		{name: "both credentials", body: `{"baseline_token":"a.b","cursor":"c.d"}`},
		{name: "both present with one null", body: `{"baseline_token":null,"cursor":"a.b"}`},
		{name: "duplicate baseline token", body: `{"baseline_token":"a.b","baseline_token":"a.b"}`},
		{name: "duplicate escaped key", body: `{"cursor":"a.b","curs\u006fr":"c.d"}`},
		{name: "duplicate page size", body: `{"cursor":"a.b","page_size":1,"page_size":2}`},
		{name: "unknown field", body: `{"cursor":"` + fixtureToken + `","after_sequence":"17"}`},
		{name: "unknown third field", body: `{"cursor":"a.b","page_size":1,"position":0}`},
		{name: "uppercase key", body: `{"CURSOR":"a.b"}`},
		{name: "case variant page size", body: `{"cursor":"a.b","Page_Size":1}`},
		{name: "null baseline token", body: `{"baseline_token":null}`, field: "baseline_token"},
		{name: "invalid baseline alphabet", body: `{"baseline_token":"a/b.c="}`, field: "baseline_token"},
		{name: "wrong cursor type", body: `{"cursor":false}`, field: "cursor"},
		{name: "blank cursor", body: `{"cursor":""}`, field: "cursor"},
		{name: "cursor with spaces", body: `{"cursor":" ` + fixtureToken + `"}`, field: "cursor"},
		{name: "cursor with newline", body: `{"cursor":"a.b\n"}`, field: "cursor"},
		{name: "cursor with extra segment", body: `{"cursor":"a.b.c"}`, field: "cursor"},
		{name: "cursor above length limit", body: `{"cursor":"A` + maxToken + `"}`, field: "cursor"},
		{name: "null page size", body: `{"cursor":"a.b","page_size":null}`, field: "page_size"},
		{name: "page size below minimum", body: `{"cursor":"a.b","page_size":0}`, field: "page_size"},
		{name: "page size above maximum", body: `{"cursor":"a.b","page_size":101}`, field: "page_size"},
		{name: "overflowing page size", body: `{"cursor":"a.b","page_size":18446744073709551616}`, field: "page_size"},
		{name: "decimal spelling", body: `{"cursor":"a.b","page_size":1.0}`, field: "page_size"},
		{name: "exponential spelling", body: `{"cursor":"a.b","page_size":5e1}`, field: "page_size"},
		{name: "quoted page size", body: `{"cursor":"a.b","page_size":"50"}`, field: "page_size"},
		{name: "nonobject body", body: "[]"},
		{name: "trailing JSON value", body: `{"cursor":"a.b"} {}`},
		{name: "invalid UTF-8", body: "{\"cursor\":\"\xff\"}"},
		{name: "unsupported media type", body: "{}", contentType: "text/plain", status: http.StatusUnsupportedMediaType, code: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "body exceeds one MiB", body: "{}" + strings.Repeat(" ", int(binding.MaxJSONBodyBytes)-1),
			status: http.StatusRequestEntityTooLarge, code: "REQUEST_BODY_TOO_LARGE"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 通过测试路由读取真实 HTTP 正文，只用合格参数模拟后续调用。
			request := httptest.NewRequest(http.MethodPost, "/fixture", strings.NewReader(testCase.body)).
				WithContext(requestcontext.WithRequestID(t.Context(), requestID))
			contentType := testCase.contentType
			if contentType == "" {
				contentType = "application/json"
			}
			request.Header.Set("Content-Type", contentType)
			router := gin.New()
			router.POST("/fixture", func(requestContext *gin.Context) {
				result, ok := readPullSyncChangesRequest(requestContext)
				if testCase.want.PageSize == 0 {
					if ok || result != nil || !requestContext.IsAborted() {
						t.Fatal("错误请求仍交付拉取参数或继续处理")
					}
					return
				}
				if !ok || result == nil || *result != testCase.want {
					t.Fatal("拉取参数未保留凭据类型、原文或页大小")
				}
				requestContext.Status(http.StatusNoContent)
			})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			// 2. 检查公开状态、错误字段和请求 ID，确保错误未回显原始凭据。
			if testCase.want.PageSize != 0 {
				if response.Code != http.StatusNoContent {
					t.Fatalf("合法输入未通过解析：%d，%s", response.Code, response.Body.String())
				}
				return
			}
			status, code := testCase.status, testCase.code
			if status == 0 {
				status, code = http.StatusBadRequest, "INVALID_ARGUMENT"
				if testCase.field == "cursor" || testCase.field == "baseline_token" {
					code = "INVALID_CURSOR"
				}
			}
			var failure dto.ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil || response.Code != status ||
				failure.Error.Code != code || failure.RequestID != requestID || response.Header().Get("Cache-Control") != "no-store" ||
				strings.Contains(response.Body.String(), fixtureToken) {
				t.Fatalf("错误响应不符：%d，%s，%v", response.Code, response.Body.String(), err)
			}
			if (testCase.field == "" && failure.Error.Details != nil) ||
				(testCase.field != "" && failure.Error.Details["field"] != testCase.field) {
				t.Fatalf("错误字段不符：%+v，预期 %q", failure.Error.Details, testCase.field)
			}
		})
	}
}
