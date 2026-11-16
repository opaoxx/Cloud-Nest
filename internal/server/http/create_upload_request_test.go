package http

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
)

func TestCreateUploadRequestHTTPFlow(t *testing.T) {
	validBody := `{"parent_id":"12","name":"资料.bin","size_bytes":2048}`
	maximumBody := validBody + strings.Repeat(" ", int(binding.MaxJSONBodyBytes)-len(validBody))
	for _, fixture := range []struct {
		name, body, field, code, contentType, encoding string
		parentID, size                                 uint64
		status                                         int
		hash, noAuthorization, badToken                bool
	}{
		{name: "required fields", body: validBody, parentID: 12, size: 2048, status: 204},
		{name: "zero bytes", body: `{"parent_id":"12","name":"资料.bin","size_bytes":0}`, parentID: 12, status: 204},
		{name: "maximum parent", body: `{"parent_id":"18446744073709551615","name":"资料.bin","size_bytes":1}`, parentID: ^uint64(0), size: 1, status: 204},
		{name: "single limit", body: `{"parent_id":"12","name":"资料.bin","size_bytes":16777216}`, parentID: 12, size: 16777216, status: 204},
		{name: "above limit for service", body: `{"parent_id":"12","name":"资料.bin","size_bytes":16777217}`, parentID: 12, size: 16777217, status: 204},
		{name: "maximum size for service", body: `{"parent_id":"12","name":"资料.bin","size_bytes":18446744073709551615}`, parentID: 12, size: ^uint64(0), status: 204},
		{name: "null hash", body: `{"parent_id":"12","name":"资料.bin","size_bytes":2048,"sha256":null}`, parentID: 12, size: 2048, status: 204},
		{name: "mixed hex hash", body: `{"parent_id":"12","name":"资料.bin","size_bytes":2048,"sha256":"` + strings.Repeat("aB", 32) + `"}`, parentID: 12, size: 2048, hash: true, status: 204},
		{name: "maximum JSON body", body: maximumBody, parentID: 12, size: 2048, status: 204},
		{name: "missing parent", body: `{"name":"资料.bin","size_bytes":0}`, field: "parent_id", status: 400},
		{name: "null parent", body: `{"parent_id":null,"name":"资料.bin","size_bytes":0}`, field: "parent_id", status: 400},
		{name: "zero parent", body: `{"parent_id":"0","name":"资料.bin","size_bytes":0}`, field: "parent_id", status: 400},
		{name: "leading zero parent", body: `{"parent_id":"012","name":"资料.bin","size_bytes":0}`, field: "parent_id", status: 400},
		{name: "numeric parent", body: `{"parent_id":12,"name":"资料.bin","size_bytes":0}`, status: 400},
		{name: "missing name", body: `{"parent_id":"12","size_bytes":0}`, field: "name", status: 400},
		{name: "null name", body: `{"parent_id":"12","name":null,"size_bytes":0}`, field: "name", status: 400},
		{name: "missing size", body: `{"parent_id":"12","name":"资料.bin"}`, field: "size_bytes", status: 400},
		{name: "null size", body: `{"parent_id":"12","name":"资料.bin","size_bytes":null}`, field: "size_bytes", status: 400},
		{name: "negative size", body: `{"parent_id":"12","name":"资料.bin","size_bytes":-1}`, status: 400},
		{name: "quoted size", body: `{"parent_id":"12","name":"资料.bin","size_bytes":"1"}`, status: 400},
		{name: "fraction size", body: `{"parent_id":"12","name":"资料.bin","size_bytes":1.0}`, status: 400},
		{name: "exponent size", body: `{"parent_id":"12","name":"资料.bin","size_bytes":1e1}`, status: 400},
		{name: "boolean size", body: `{"parent_id":"12","name":"资料.bin","size_bytes":true}`, status: 400},
		{name: "overflow size", body: `{"parent_id":"12","name":"资料.bin","size_bytes":18446744073709551616}`, status: 400},
		{name: "empty hash", body: `{"parent_id":"12","name":"资料.bin","size_bytes":0,"sha256":""}`, field: "sha256", status: 400},
		{name: "invalid hex hash", body: `{"parent_id":"12","name":"资料.bin","size_bytes":0,"sha256":"` + strings.Repeat("g", 64) + `"}`, field: "sha256", status: 400},
		{name: "short hash", body: `{"parent_id":"12","name":"资料.bin","size_bytes":0,"sha256":"` + strings.Repeat("a", 63) + `"}`, field: "sha256", status: 400},
		{name: "numeric hash", body: `{"parent_id":"12","name":"资料.bin","size_bytes":0,"sha256":1}`, status: 400},
		{name: "unknown identity", body: `{"parent_id":"12","name":"资料.bin","size_bytes":0,"user_id":99}`, status: 400},
		{name: "unknown upload mode", body: `{"parent_id":"12","name":"资料.bin","size_bytes":0,"upload_mode":"MULTIPART"}`, status: 400},
		{name: "malformed JSON", body: `{"parent_id":"private-parent"`, status: 400},
		{name: "second JSON", body: validBody + `{}`, status: 400},
		{name: "invalid UTF-8", body: `{"parent_id":"12","name":"` + string([]byte{0xff}) + `","size_bytes":0}`, status: 400},
		{name: "large JSON body", body: maximumBody + " ", status: 413, code: "REQUEST_BODY_TOO_LARGE"},
		{name: "unsupported type", body: validBody, contentType: "text/plain", status: 415, code: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "compressed body", body: validBody, encoding: "gzip", status: 415, code: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "authorization before JSON", body: `{`, noAuthorization: true, status: 401, code: "AUTH_REQUIRED"},
		{name: "invalid authorization before fields", body: `{}`, badToken: true, status: 401, code: "AUTH_REQUIRED"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 认证先行，再使用既有 JSON 读取器和上传请求解析。
			var logs bytes.Buffer
			var accepted *domain.CreateUploadInput
			readCalls, downstream := 0, 0
			router := NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
			authorize := func(_ context.Context, token string) (*domain.Actor, error) {
				if token != "fixture-access" {
					return nil, domain.ErrInvalidAccessToken
				}
				return &domain.Actor{UserID: 7, DeviceID: 19, SessionID: 17}, nil
			}
			router.POST("/test-create-upload-request", middleware.RequireAuthorization(authorize, handler.AuthorizationFailure), func(requestContext *gin.Context) {
				readCalls++
				var request dto.CreateUploadRequest
				if !binding.ReadJSON(requestContext, &request) {
					return
				}
				input, fieldError := dto.ParseCreateUploadRequest(request)
				if fieldError != nil {
					handler.InvalidArgument(requestContext, fieldError)
					return
				}
				accepted = input
				requestContext.AbortWithStatus(204)
			}, func(*gin.Context) { downstream++ })
			request := httptest.NewRequest("POST", "/test-create-upload-request?user_id=99&note=private-query", strings.NewReader(fixture.body))
			contentType := fixture.contentType
			if contentType == "" {
				contentType = "application/json"
			}
			request.Header.Set("Content-Type", contentType)
			if fixture.encoding != "" {
				request.Header.Set("Content-Encoding", fixture.encoding)
			}
			request.Header.Set("Origin", "http://127.0.0.1:5173")
			request.Header.Set("X-Request-ID", "28EE758A-0727-42DE-AE7D-3BB33155E148")
			if !fixture.noAuthorization {
				token := "fixture-access"
				if fixture.badToken {
					token = "private-access"
				}
				request.Header.Set("Authorization", "Bearer "+token)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			// 2. 核对完整数值、可空摘要、固定错误和中间件关联。
			if response.Code != fixture.status || downstream != 0 || response.Header().Get("X-Request-ID") != "28ee758a-0727-42de-ae7d-3bb33155e148" || response.Header().Get("Access-Control-Allow-Origin") != "http://127.0.0.1:5173" {
				t.Fatal("上传请求状态或关联错误", response.Code)
			}
			if fixture.status == 401 && readCalls != 0 {
				t.Fatal("认证失败后仍读取正文")
			}
			if fixture.status == 204 {
				if accepted == nil || accepted.ParentID != fixture.parentID || accepted.Name != "资料.bin" || accepted.SizeBytes != fixture.size || (accepted.ExpectedSHA256 != nil) != fixture.hash || accepted.IsValid() != (fixture.size <= domain.MaxSingleUploadSizeBytes) || response.Body.Len() != 0 {
					t.Fatal("上传声明、大小或摘要解析错误")
				}
				if fixture.hash && accepted.ExpectedSHA256[0] != 0xab {
					t.Fatal("实际请求的十六进制摘要转换错误")
				}
			} else {
				code := fixture.code
				if code == "" {
					code = "INVALID_ARGUMENT"
				}
				var body dto.ErrorResponse
				if accepted != nil || json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Error.Code != code || body.RequestID != response.Header().Get("X-Request-ID") || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("上传请求错误交付不符")
				}
				if fixture.field == "" {
					if body.Error.Details != nil {
						t.Fatal("绑定或认证错误的 details 不符")
					}
				} else if len(body.Error.Details) != 1 || body.Error.Details["field"] != fixture.field {
					t.Fatal("上传请求字段定位错误")
				}
			}
			if strings.Contains(logs.String(), "private-") || strings.Contains(response.Body.String(), "private-") || strings.Contains(logs.String(), "资料.bin") || strings.Contains(logs.String(), strings.Repeat("aB", 32)) {
				t.Fatal("上传声明或凭证原文进入日志或错误")
			}
		})
	}
}
