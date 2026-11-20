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

func TestRestoreEntryRequestHTTP(t *testing.T) {
	const path = "/test-restore-entry-request"
	const versionField = "expected_revision"
	const versionMessage = "预期版本须为 1～18446744073709551615 的标准十进制整数"
	const validBody = `{"expected_revision":"4"}`
	var accepted *dto.RestoreEntryInput
	router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })

	// 1. 通过测试入口串联 JSON 绑定、版本转换及固定字段错误。
	router.POST(path, func(requestContext *gin.Context) {
		var request dto.RestoreEntryRequest
		if !binding.ReadJSON(requestContext, &request) {
			return
		}
		input, fieldError := dto.ParseRestoreEntryRequest(request)
		if fieldError != nil {
			handler.InvalidArgument(requestContext, fieldError)
			return
		}
		accepted = input
		requestContext.Status(http.StatusNoContent)
	})

	// 2. 核对完整整数范围、字段错误和正文绑定错误。
	for _, testCase := range []struct {
		name, body, field string
		status            int
		revision          uint64
		contentType       string
	}{
		{name: "standard", body: validBody, status: 204, revision: 4},
		{name: "minimum", body: `{"expected_revision":"1"}`, status: 204, revision: 1},
		{name: "large exact integer", body: `{"expected_revision":"9007199254740993"}`, status: 204, revision: 9007199254740993},
		{name: "maximum", body: `{"expected_revision":"18446744073709551615"}`, status: 204, revision: math.MaxUint64},
		{name: "missing revision", body: `{}`, status: 400, field: versionField},
		{name: "null revision", body: `{"expected_revision":null}`, status: 400, field: versionField},
		{name: "empty revision", body: `{"expected_revision":""}`, status: 400, field: versionField},
		{name: "zero", body: `{"expected_revision":"0"}`, status: 400, field: versionField},
		{name: "leading zero", body: `{"expected_revision":"04"}`, status: 400, field: versionField},
		{name: "positive sign", body: `{"expected_revision":"+4"}`, status: 400, field: versionField},
		{name: "negative sign", body: `{"expected_revision":"-4"}`, status: 400, field: versionField},
		{name: "spaces", body: `{"expected_revision":" 4 "}`, status: 400, field: versionField},
		{name: "decimal", body: `{"expected_revision":"4.0"}`, status: 400, field: versionField},
		{name: "non ASCII digit", body: `{"expected_revision":"４"}`, status: 400, field: versionField},
		{name: "overflow", body: `{"expected_revision":"18446744073709551616"}`, status: 400, field: versionField},
		{name: "number instead of string", body: `{"expected_revision":4}`, status: 400},
		{name: "unknown identity", body: `{"expected_revision":"4","user_id":"7"}`, status: 400},
		{name: "unknown destination", body: `{"expected_revision":"4","target_parent_id":"12"}`, status: 400},
		{name: "broken JSON", body: `{"expected_revision":`, status: 400},
		{name: "wrong media type", body: validBody, status: 415, contentType: "text/plain"},
		{name: "body over limit", body: strings.Repeat(" ", int(binding.MaxJSONBodyBytes)) + validBody, status: 413},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			accepted = nil
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(testCase.body))
			contentType := testCase.contentType
			if contentType == "" {
				contentType = "application/json"
			}
			request.Header.Set("Content-Type", contentType)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != testCase.status {
				t.Fatalf("响应状态为 %d，期望 %d", response.Code, testCase.status)
			}
			if response.Code == http.StatusNoContent {
				if accepted == nil || accepted.ExpectedRevision != testCase.revision || response.Body.Len() != 0 {
					t.Fatalf("转换结果为 %+v，期望整数 %d", accepted, testCase.revision)
				}
				return
			}

			// 3. 校验失败时停止转换，并区分版本字段错误和共享绑定错误。
			codes := map[int]string{400: "INVALID_ARGUMENT", 413: "REQUEST_BODY_TOO_LARGE", 415: "UNSUPPORTED_MEDIA_TYPE"}
			var failure dto.ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil || accepted != nil ||
				failure.Error.Code != codes[testCase.status] || failure.RequestID == "" ||
				failure.RequestID != response.Header().Get("X-Request-ID") || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("错误正文为 %s，解码错误为 %v", response.Body.String(), err)
			}
			if testCase.field == "" {
				if failure.Error.Details != nil {
					t.Fatalf("绑定错误的 details 为 %v，期望 null", failure.Error.Details)
				}
			} else if failure.Error.Details["field"] != testCase.field || failure.Error.Message != versionMessage {
				t.Fatalf("版本字段错误为 %+v", failure.Error)
			}
		})
	}
}
