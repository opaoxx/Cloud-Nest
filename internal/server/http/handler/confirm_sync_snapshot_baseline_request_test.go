package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

func TestReadConfirmSyncSnapshotBaselineRequest(t *testing.T) {
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	const snapshotID = "052dc4f7-f4c1-46b3-a509-bf96992f6580"
	const token = "SyntheticSecret_b-9.C_d-8"
	maxToken := strings.Repeat("a", domain.MaxSyncSnapshotTokenLength-2) + ".b"
	for _, testCase := range []struct {
		name, snapshotID, body, token string
		contentType, contentEncoding  string
		status                        int
		errorCode, field              string
	}{
		{name: "ordinary token", body: `{"baseline_token":"` + token + `"}`, token: token},
		{name: "preserves decoded value", body: `{"baseline_toke\u006e":"SyntheticSecret_b-9\u002eC_d-8"}`, token: token, contentType: "application/json; charset=UTF-8"},
		{name: "1024-character token", body: `{"baseline_token":"` + maxToken + `"}`, token: maxToken},
		{name: "upper-case ID before body", snapshotID: strings.ToUpper(snapshotID), body: "!", field: "snapshot_id"},
		{name: "wrong UUID version", snapshotID: "052dc4f7-f4c1-16b3-a509-bf96992f6580", field: "snapshot_id"},
		{name: "wrong UUID variant", snapshotID: "052dc4f7-f4c1-46b3-0509-bf96992f6580", field: "snapshot_id"},
		{name: "missing token", body: "{}", field: "baseline_token"},
		{name: "null token", body: `{"baseline_token":null}`, field: "baseline_token"},
		{name: "number token", body: `{"baseline_token":17}`, field: "baseline_token"},
		{name: "empty token", body: `{"baseline_token":""}`, field: "baseline_token"},
		{name: "whitespace token", body: `{"baseline_token":" ` + token + `"}`, field: "baseline_token"},
		{name: "line break in token", body: `{"baseline_token":"` + token + `\n"}`, field: "baseline_token"},
		{name: "forbidden alphabet", body: `{"baseline_token":"a/b.c="}`, field: "baseline_token"},
		{name: "extra token segment", body: `{"baseline_token":"a.b.c"}`, field: "baseline_token"},
		{name: "1025-character token", body: `{"baseline_token":"A` + maxToken + `"}`, field: "baseline_token"},
		{name: "duplicate key", body: `{"baseline_token":"` + token + `","baseline_token":"` + token + `"}`},
		{name: "duplicate through escaped key", body: `{"baseline_token":"` + token + `","baseline_toke\u006e":"x.y"}`},
		{name: "unknown field", body: `{"baseline_token":"` + token + `","ack_sequence":"17"}`},
		{name: "case variant key", body: `{"Baseline_Token":"` + token + `"}`},
		{name: "empty body"},
		{name: "array body", body: "[]"},
		{name: "malformed body", body: `{"baseline_token":}`},
		{name: "second JSON value", body: `{"baseline_token":"` + token + `"} {}`},
		{name: "invalid UTF-8", body: "{\"baseline_token\":\"\xff\"}"},
		{name: "wrong media type", body: "{}", contentType: "text/plain", status: http.StatusUnsupportedMediaType, errorCode: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "compressed request", body: "{}", contentEncoding: "gzip", status: http.StatusUnsupportedMediaType, errorCode: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "body above 1 MiB", body: "{}" + strings.Repeat(" ", int(binding.MaxJSONBodyBytes)-1), status: http.StatusRequestEntityTooLarge, errorCode: "REQUEST_BODY_TOO_LARGE"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 通过独立 POST 路由传入合成的路径与正文。
			pathID := testCase.snapshotID
			if pathID == "" {
				pathID = snapshotID
			}
			request := httptest.NewRequest(http.MethodPost, "/fixture/"+pathID+"/confirm", strings.NewReader(testCase.body)).
				WithContext(requestcontext.WithRequestID(t.Context(), requestID))
			contentType := testCase.contentType
			if contentType == "" {
				contentType = "application/json"
			}
			request.Header.Set("Content-Type", contentType)
			if testCase.contentEncoding != "" {
				request.Header.Set("Content-Encoding", testCase.contentEncoding)
			}

			// 2. 验证只在解析成功时取得完整参数。
			router := gin.New()
			router.POST("/fixture/:snapshot_id/confirm", func(requestContext *gin.Context) {
				parsedID, parsedToken, ok := readConfirmSyncSnapshotBaselineRequest(requestContext)
				if testCase.token == "" {
					if ok || parsedID != uuid.Nil || parsedToken != "" || !requestContext.IsAborted() {
						t.Fatal("错误输入仍交付确认参数或继续处理")
					}
					return
				}
				if !ok || parsedID.String() != snapshotID || parsedToken != testCase.token {
					t.Fatal("未原样交付预期的快照 ID 与凭据文本")
				}
				requestContext.Status(http.StatusNoContent)
			})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			// 3. 核对错误种类、字段、请求 ID 和禁止缓存，错误正文不回显凭据。
			if testCase.token != "" {
				if response.Code != http.StatusNoContent {
					t.Fatalf("成功夹具响应为 %d，正文 %s", response.Code, response.Body.String())
				}
				return
			}
			status, errorCode := testCase.status, testCase.errorCode
			if status == 0 {
				status, errorCode = http.StatusBadRequest, "INVALID_ARGUMENT"
				if testCase.field == "baseline_token" {
					errorCode = "INVALID_CURSOR"
				}
			}
			var responseBody dto.ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &responseBody); err != nil || response.Code != status ||
				responseBody.Error.Code != errorCode || responseBody.RequestID != requestID ||
				response.Header().Get("Cache-Control") != "no-store" || strings.Contains(response.Body.String(), token) {
				t.Fatalf("错误响应为 %d，正文 %s", response.Code, response.Body.String())
			}
			if (testCase.field == "" && responseBody.Error.Details != nil) ||
				(testCase.field != "" && responseBody.Error.Details["field"] != testCase.field) {
				t.Fatalf("错误详情为 %+v，期望字段 %q", responseBody.Error.Details, testCase.field)
			}
		})
	}
}
