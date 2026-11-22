package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

func TestReadCreateSyncSnapshotRequest(t *testing.T) {
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	const fixtureKey = "Snapshot-Create-A"
	for _, testCase := range []struct {
		name, body, contentType string
		keys                    []string
		pageSize, status        int
		errorCode, field        string
	}{
		{name: "default size", body: "{}", pageSize: 50},
		{name: "explicit default", body: `{"page_size":50}`, pageSize: 50},
		{name: "minimum", body: `{"page_size": 1}`, pageSize: 1, contentType: "application/json; charset=UTF-8"},
		{name: "maximum with escaped field name", body: `{"page_siz\u0065":100}`, pageSize: 100},
		{name: "key before malformed JSON", body: "!", keys: []string{}, field: "Idempotency-Key"},
		{name: "repeated key", body: "{}", keys: []string{"one", "two"}, field: "Idempotency-Key"},
		{name: "merged keys", body: "{}", keys: []string{"one,two"}, field: "Idempotency-Key"},
		{name: "invalid key characters", body: "{}", keys: []string{"合成键"}, field: "Idempotency-Key"},
		{name: "null size", body: `{"page_size":null}`, field: "page_size"},
		{name: "zero size", body: `{"page_size":0}`, field: "page_size"},
		{name: "negative size", body: `{"page_size":-1}`, field: "page_size"},
		{name: "above maximum", body: `{"page_size":101}`, field: "page_size"},
		{name: "overflow", body: `{"page_size":18446744073709551616}`, field: "page_size"},
		{name: "fractional size", body: `{"page_size":1.5}`, field: "page_size"},
		{name: "decimal spelling", body: `{"page_size":50.0}`, field: "page_size"},
		{name: "exponent spelling", body: `{"page_size":5e1}`, field: "page_size"},
		{name: "quoted size", body: `{"page_size":"50"}`, field: "page_size"},
		{name: "boolean size", body: `{"page_size":false}`, field: "page_size"},
		{name: "object size", body: `{"page_size":{}}`, field: "page_size"},
		{name: "duplicate size", body: `{"page_size":1,"page_size":2}`},
		{name: "duplicate through escaped name", body: `{"page_size":1,"page_siz\u0065":2}`},
		{name: "unknown field", body: `{"limit":50}`},
		{name: "unknown field with page size", body: `{"page_size":50,"limit":50}`},
		{name: "case variant field", body: `{"PAGE_SIZE":50}`},
		{name: "body absent"},
		{name: "null body", body: "null"},
		{name: "array body", body: "[]"},
		{name: "malformed object", body: `{"page_size":}`},
		{name: "trailing object", body: "{} {}"},
		{name: "invalid UTF-8", body: "{\"bad\xff\":1}"},
		{name: "wrong media type", body: "{}", contentType: "text/plain",
			status: http.StatusUnsupportedMediaType, errorCode: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "too many bytes", body: "{}" + strings.Repeat(" ", int(binding.MaxJSONBodyBytes)-1),
			status: http.StatusRequestEntityTooLarge, errorCode: "REQUEST_BODY_TOO_LARGE"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 在独立入口构造提供给解析方法的 HTTP 请求。
			request := httptest.NewRequest(http.MethodPost, "/fixture", strings.NewReader(testCase.body)).
				WithContext(requestcontext.WithRequestID(t.Context(), requestID))
			contentType := testCase.contentType
			if contentType == "" {
				contentType = "application/json"
			}
			request.Header.Set("Content-Type", contentType)
			keys := testCase.keys
			if keys == nil {
				keys = []string{"  " + fixtureKey + "\t"}
			}
			for _, key := range keys {
				request.Header.Add("Idempotency-Key", key)
			}
			router := gin.New()
			router.POST("/fixture", func(requestContext *gin.Context) {
				// 2. 创建端只能在解析完整成功时取得两个参数。
				pageSize, key, ok := readCreateSyncSnapshotRequest(requestContext)
				if testCase.pageSize == 0 {
					if ok || pageSize != 0 || key != "" || !requestContext.IsAborted() {
						t.Fatalf("错误输入仍交付创建参数：%d, %q, %t", pageSize, key, ok)
					}
					return
				}
				if !ok || pageSize != testCase.pageSize || key != fixtureKey {
					t.Fatalf("创建参数为 %d, %q, %t，期望页大小 %d 和键 %q", pageSize, key, ok, testCase.pageSize, fixtureKey)
				}
				requestContext.Status(http.StatusNoContent)
			})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			// 3. 核对字段错误和正文错误的公开响应。
			if testCase.pageSize != 0 {
				if response.Code != http.StatusNoContent {
					t.Fatalf("合法请求响应为 %d，正文 %s", response.Code, response.Body.String())
				}
				return
			}
			status, errorCode := testCase.status, testCase.errorCode
			if status == 0 {
				status, errorCode = http.StatusBadRequest, "INVALID_ARGUMENT"
			}
			var body dto.ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != status ||
				body.Error.Code != errorCode || body.RequestID != requestID || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("错误响应为 %d，正文 %s", response.Code, response.Body.String())
			}
			if (testCase.field == "" && body.Error.Details != nil) ||
				(testCase.field != "" && body.Error.Details["field"] != testCase.field) {
				t.Fatalf("错误详情为 %+v，期望字段 %q", body.Error.Details, testCase.field)
			}
		})
	}
}
