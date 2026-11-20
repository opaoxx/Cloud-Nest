package http

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
)

func TestTrashEntryRequestHTTP(t *testing.T) {
	// 1. 用测试入口串联共享 JSON 读取器、版本校验与固定错误交付。
	const path = "/test-trash-entry-request"
	const validBody = `{"expected_revision":"3"}`
	const versionField = "expected_revision"
	const versionMessage = "预期版本须为 1～18446744073709551615 的标准十进制整数"
	var accepted *dto.TrashEntryInput
	router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
	router.POST(path, func(requestContext *gin.Context) {
		var request dto.TrashEntryRequest
		if !binding.ReadJSON(requestContext, &request) {
			return
		}
		input, fieldError := dto.ParseTrashEntryRequest(request)
		if fieldError != nil {
			handler.InvalidArgument(requestContext, fieldError)
			return
		}
		accepted = input
		requestContext.Status(http.StatusNoContent)
	})

	// 2. 逐项验证版本边界、字段错误和共享正文约束。
	for _, scenario := range []struct {
		name, body, field, contentType, contentEncoding string
		status                                          int
		revision                                        uint64
	}{
		{name: "standard version", body: validBody, status: 204, revision: 3},
		{name: "minimum version", body: `{"expected_revision":"1"}`, status: 204, revision: 1},
		{name: "beyond JavaScript safe integer", body: `{"expected_revision":"9007199254740993"}`, status: 204, revision: 9007199254740993},
		{name: "maximum version", body: `{"expected_revision":"18446744073709551615"}`, status: 204, revision: math.MaxUint64},
		{name: "omitted", body: `{}`, field: versionField, status: 400},
		{name: "null", body: `{"expected_revision":null}`, field: versionField, status: 400},
		{name: "empty", body: `{"expected_revision":""}`, field: versionField, status: 400},
		{name: "zero", body: `{"expected_revision":"0"}`, field: versionField, status: 400},
		{name: "leading zero", body: `{"expected_revision":"03"}`, field: versionField, status: 400},
		{name: "plus sign", body: `{"expected_revision":"+3"}`, field: versionField, status: 400},
		{name: "negative", body: `{"expected_revision":"-3"}`, field: versionField, status: 400},
		{name: "surrounding whitespace", body: `{"expected_revision":" 3 "}`, field: versionField, status: 400},
		{name: "newline", body: `{"expected_revision":"3\n"}`, field: versionField, status: 400},
		{name: "decimal", body: `{"expected_revision":"3.0"}`, field: versionField, status: 400},
		{name: "exponent", body: `{"expected_revision":"3e1"}`, field: versionField, status: 400},
		{name: "non ASCII digit", body: `{"expected_revision":"３"}`, field: versionField, status: 400},
		{name: "uint64 overflow", body: `{"expected_revision":"18446744073709551616"}`, field: versionField, status: 400},
		{name: "number instead of string", body: `{"expected_revision":3}`, status: 400},
		{name: "boolean instead of string", body: `{"expected_revision":true}`, status: 400},
		{name: "unknown field", body: `{"expected_revision":"3","user_id":"23"}`, status: 400},
		{name: "non-object root", body: `null`, status: 400},
		{name: "trailing object", body: validBody + ` {}`, status: 400},
		{name: "broken JSON", body: `{"expected_revision":`, status: 400},
		{name: "invalid UTF-8", body: "{\"expected_revision\":\"\xff\"}", status: 400},
		{name: "unsupported media type", body: validBody, contentType: "text/plain", status: 415},
		{name: "compressed body", body: validBody, contentEncoding: "gzip", status: 415},
		{name: "too large", body: strings.Repeat(" ", int(binding.MaxJSONBodyBytes)) + validBody, status: 413},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			accepted = nil
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(scenario.body))
			contentType := scenario.contentType
			if contentType == "" {
				contentType = "application/json"
			}
			request.Header.Set("Content-Type", contentType)
			if scenario.contentEncoding != "" {
				request.Header.Set("Content-Encoding", scenario.contentEncoding)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != scenario.status {
				t.Fatalf("请求状态 %d，预期 %d", response.Code, scenario.status)
			}
			if scenario.status == http.StatusNoContent {
				if accepted == nil || accepted.ExpectedRevision != scenario.revision || response.Body.Len() != 0 {
					t.Fatal("合法版本未被完整转换", accepted)
				}
				return
			}

			// 3. 确保非法请求未进入成功分支，错误可用请求 ID 关联。
			expectedCodes := map[int]string{
				400: "INVALID_ARGUMENT", 413: "REQUEST_BODY_TOO_LARGE", 415: "UNSUPPORTED_MEDIA_TYPE",
			}
			var result dto.ErrorResponse
			if accepted != nil || json.Unmarshal(response.Body.Bytes(), &result) != nil ||
				result.Error.Code != expectedCodes[scenario.status] || result.RequestID == "" ||
				result.RequestID != response.Header().Get("X-Request-ID") ||
				response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("回收正文错误响应不符", result)
			}
			if scenario.field == "" {
				if result.Error.Details != nil {
					t.Fatal("共享读取错误的 details 须为 null")
				}
			} else if result.Error.Details["field"] != scenario.field || result.Error.Message != versionMessage {
				t.Fatal("非法版本未交付固定的字段错误", result.Error)
			}
		})
	}
}
