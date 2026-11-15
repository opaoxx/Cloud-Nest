package http

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestCreateDirectoryRequestHashHTTPFlow(t *testing.T) {
	for _, testCase := range []struct {
		name, first, next string
		same              bool
	}{
		{name: "field order whitespace and omitted color",
			first: `{"parent_id":"11","name":"学习资料"}`,
			next:  "{\n  \"color_key\":null, \"name\":\"学习资料\", \"parent_id\":\"11\"\n}", same: true},
		{name: "display trim", first: `{"parent_id":"11","name":"学习资料"}`,
			next: `{"parent_id":"11","name":" \u3000学习资料\u00a0 "}`, same: true},
		{name: "unicode json escape", first: `{"parent_id":"11","name":"学习资料"}`,
			next: `{"parent_id":"11","name":"\u5b66\u4e60资料"}`, same: true},
		{name: "null and empty color", first: `{"parent_id":"11","name":"学习资料","color_key":null}`,
			next: `{"parent_id":"11","name":"学习资料","color_key":""}`},
		{name: "display case", first: `{"parent_id":"11","name":"Notes"}`,
			next: `{"parent_id":"11","name":"NOTES"}`},
		{name: "display unicode spelling", first: `{"parent_id":"11","name":"Café"}`,
			next: `{"parent_id":"11","name":"Cafe\u0301"}`},
		{name: "full parent precision", first: `{"parent_id":"9007199254740992","name":"资料"}`,
			next: `{"parent_id":"9007199254740993","name":"资料"}`},
		{name: "html json escape", first: `{"parent_id":"11","name":"A&B"}`,
			next: `{"parent_id":"11","name":"A\u0026B"}`, same: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var hashes [2][sha256.Size]byte
			for index, body := range []string{testCase.first, testCase.next} {
				// 1. 测试入口从认证、请求头和 JSON 解析得到实际业务参数。
				var logs bytes.Buffer
				router := NewRouter(logging.New(&logs), nil, func(context.Context) bool { return true })
				protected := middleware.RequireAuthorization(func(context.Context, string) (*domain.Actor, error) {
					return &domain.Actor{UserID: 7, DeviceID: 13, SessionID: 17}, nil
				}, handler.AuthorizationFailure)
				router.POST("/test-directory-hash", protected, func(requestContext *gin.Context) {
					if _, fieldError := dto.ParseIdempotencyKey(requestContext.Request); fieldError != nil {
						handler.InvalidArgument(requestContext, fieldError)
						return
					}
					var request dto.CreateDirectoryRequest
					if !binding.ReadJSON(requestContext, &request) {
						return
					}
					input, fieldError := dto.ParseCreateDirectoryRequest(request)
					if fieldError != nil {
						handler.InvalidArgument(requestContext, fieldError)
						return
					}
					hash, err := service.BuildCreateDirectoryRequestHash(input.ParentID, input.Name, input.ColorKey)
					if err != nil {
						t.Error("有效请求的摘要生成失败", err)
						requestContext.AbortWithStatus(500)
						return
					}
					hashes[index] = hash
					requestContext.AbortWithStatus(204)
				})
				request := httptest.NewRequest("POST", "/test-directory-hash?name=private-query", strings.NewReader(body))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Authorization", "Bearer fixture-access")
				keys := []string{"Request-A", "Request-B"}
				requestIDs := []string{"28ee758a-0727-42de-ae7d-3bb33155e148", "28ee758a-0727-42de-ae7d-3bb33155e149"}
				request.Header.Set("Idempotency-Key", keys[index])
				request.Header.Set("X-Request-ID", requestIDs[index])
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				if response.Code != 204 || response.Body.Len() != 0 || hashes[index] == [sha256.Size]byte{} || response.Header().Get("X-Request-ID") != requestIDs[index] {
					t.Fatal("请求绑定或摘要交付测试错误", response.Code)
				}
				if strings.Contains(logs.String(), "private-query") || strings.Contains(logs.String(), "Request-A") || strings.Contains(logs.String(), "学习资料") {
					t.Fatal("摘要测试入口日志回显了参数或幂等键")
				}
			}
			// 2. 以实际字段值比较摘要，核对参数归一化与展示差异。
			if (hashes[0] == hashes[1]) != testCase.same {
				t.Fatal("HTTP 请求参数对应的摘要关系错误")
			}
		})
	}
}
