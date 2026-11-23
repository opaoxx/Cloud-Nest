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

func TestReadConfirmSyncChangesRequest(t *testing.T) {
	const requestID = "eb9c7469-d6e4-4f95-991c-a8341bcd8755"
	const path = "/test-sync-confirm"
	const cursor = "SyntheticCursor_a-9.B_d-8"
	const body = `{"cursor":"` + cursor + `"}`
	maxCursor := strings.Repeat("a", domain.MaxSyncSnapshotTokenLength-2) + ".b"
	bodyLimit := int(binding.MaxJSONBodyBytes)
	for _, scenario := range []struct {
		name, body, cursor           string
		contentType, contentEncoding string
		status                       int
		code                         string
	}{
		{name: "valid cursor", body: body, cursor: cursor},
		{name: "decoded key and cursor text", body: `{"curs\u006fr":"SyntheticCursor_a-9\u002eB_d-8"}`,
			cursor: cursor, contentType: "application/json; charset=UTF-8"},
		{name: "maximum length cursor", body: `{"cursor":"` + maxCursor + `"}`, cursor: maxCursor},
		{name: "body at one MiB", body: body + strings.Repeat(" ", bodyLimit-len(body)), cursor: cursor},
		{name: "missing cursor", body: "{}", code: "INVALID_CURSOR"},
		{name: "null cursor", body: `{"cursor":null}`, code: "INVALID_CURSOR"},
		{name: "number cursor", body: `{"cursor":17}`, code: "INVALID_CURSOR"},
		{name: "object cursor", body: `{"cursor":{"next_cursor":"a.b"}}`, code: "INVALID_CURSOR"},
		{name: "empty cursor", body: `{"cursor":""}`, code: "INVALID_CURSOR"},
		{name: "leading whitespace in cursor", body: `{"cursor":" ` + cursor + `"}`, code: "INVALID_CURSOR"},
		{name: "trailing newline in cursor", body: `{"cursor":"` + cursor + `\n"}`, code: "INVALID_CURSOR"},
		{name: "overlong cursor", body: `{"cursor":"a` + maxCursor + `"}`, code: "INVALID_CURSOR"},
		{name: "bad alphabet or padding", body: `{"cursor":"a/b.c="}`, code: "INVALID_CURSOR"},
		{name: "extra cursor segment", body: `{"cursor":"a.b.c"}`, code: "INVALID_CURSOR"},
		{name: "duplicate cursor", body: `{"cursor":"a.b","cursor":"c.d"}`},
		{name: "duplicate escaped cursor key", body: `{"cursor":"a.b","curs\u006fr":"c.d"}`},
		{name: "extra acknowledgment position", body: `{"cursor":"` + cursor + `","ack_sequence":"19"}`},
		{name: "baseline token in body", body: `{"baseline_token":"a.b"}`},
		{name: "case variant key", body: `{"Cursor":"a.b"}`},
		{name: "empty body"},
		{name: "nonobject body", body: "[]"},
		{name: "malformed body", body: `{"cursor":}`},
		{name: "trailing JSON value", body: body + " {}"},
		{name: "invalid UTF-8", body: "{\"cursor\":\"\xff\"}"},
		{name: "unsupported media type", body: body, contentType: "text/plain", status: 415, code: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "compressed body", body: body, contentEncoding: "gzip", status: 415, code: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "body above one MiB", body: body + strings.Repeat(" ", bodyLimit-len(body)+1), status: 413, code: "REQUEST_BODY_TOO_LARGE"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 将合成正文送到独立路由，只在解析成功时交付参数。
			router := gin.New()
			router.POST(path, func(requestContext *gin.Context) {
				parsed, ok := readConfirmSyncChangesRequest(requestContext)
				if scenario.cursor == "" {
					if ok || parsed != "" || !requestContext.IsAborted() {
						t.Fatal("错误输入仍交付游标或继续处理")
					}
					return
				}
				if !ok || parsed != scenario.cursor {
					t.Fatal("未保留解码后的游标原文")
				}
				requestContext.Status(http.StatusNoContent)
			})
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(scenario.body)).
				WithContext(requestcontext.WithRequestID(t.Context(), requestID))
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

			// 2. 核对状态、业务码、字段和请求 ID，拒绝时不得回显凭据。
			if scenario.cursor != "" {
				if response.Code != http.StatusNoContent || response.Body.Len() != 0 {
					t.Fatalf("成功夹具应返回无正文的 204，实际 %d", response.Code)
				}
				return
			}
			status, code := scenario.status, scenario.code
			if status == 0 {
				status = http.StatusBadRequest
			}
			if code == "" {
				code = "INVALID_ARGUMENT"
			}
			var failure dto.ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil || response.Code != status ||
				failure.RequestID != requestID || failure.Error.Code != code || response.Header().Get("Cache-Control") != "no-store" ||
				strings.Contains(response.Body.String(), cursor) {
				t.Fatalf("响应 %d 不符：%s，错误 %v", response.Code, response.Body.String(), err)
			}
			if (code == "INVALID_CURSOR" && (len(failure.Error.Details) != 1 || failure.Error.Details["field"] != "cursor")) ||
				(code != "INVALID_CURSOR" && failure.Error.Details != nil) {
				t.Fatalf("错误字段不符：%+v", failure.Error.Details)
			}
		})
	}
}
