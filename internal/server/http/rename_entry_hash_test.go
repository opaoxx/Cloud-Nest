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

func TestRenameEntryRequestHashHTTPFlow(t *testing.T) {
	for _, testCase := range []struct {
		name, firstID, nextID, first, next string
		same                               bool
	}{
		{name: "field order and whitespace", firstID: "11", nextID: "11",
			first: `{"name":"学习资料","expected_revision":"3"}`,
			next:  "{\n  \"expected_revision\":\"3\", \"name\":\"学习资料\"\n}", same: true},
		{name: "display trim", firstID: "11", nextID: "11",
			first: `{"name":"学习资料","expected_revision":"3"}`,
			next:  `{"name":" \u3000学习资料\u00a0 ","expected_revision":"3"}`, same: true},
		{name: "unicode json escape", firstID: "11", nextID: "11",
			first: `{"name":"学习资料","expected_revision":"3"}`,
			next:  `{"name":"\u5b66\u4e60资料","expected_revision":"3"}`, same: true},
		{name: "html json escape", firstID: "11", nextID: "11",
			first: `{"name":"A&B","expected_revision":"3"}`,
			next:  `{"name":"A\u0026B","expected_revision":"3"}`, same: true},
		{name: "target", firstID: "11", nextID: "12",
			first: `{"name":"资料","expected_revision":"3"}`,
			next:  `{"name":"资料","expected_revision":"3"}`},
		{name: "revision", firstID: "11", nextID: "11",
			first: `{"name":"资料","expected_revision":"3"}`,
			next:  `{"name":"资料","expected_revision":"4"}`},
		{name: "display case", firstID: "11", nextID: "11",
			first: `{"name":"Notes","expected_revision":"3"}`,
			next:  `{"name":"NOTES","expected_revision":"3"}`},
		{name: "display spelling", firstID: "11", nextID: "11",
			first: `{"name":"Café","expected_revision":"3"}`,
			next:  `{"name":"Cafe\u0301","expected_revision":"3"}`},
		{name: "fold equivalent", firstID: "11", nextID: "11",
			first: `{"name":"Straße","expected_revision":"3"}`,
			next:  `{"name":"STRASSE","expected_revision":"3"}`},
		{name: "high target precision", firstID: "9007199254740992", nextID: "9007199254740993",
			first: `{"name":"资料","expected_revision":"3"}`,
			next:  `{"name":"资料","expected_revision":"3"}`},
		{name: "high revision precision", firstID: "11", nextID: "11",
			first: `{"name":"资料","expected_revision":"9007199254740992"}`,
			next:  `{"name":"资料","expected_revision":"9007199254740993"}`},
		{name: "maximum values", firstID: "18446744073709551615", nextID: "18446744073709551615",
			first: `{"name":"资料","expected_revision":"18446744073709551615"}`,
			next:  `{"name":"资料","expected_revision":"18446744073709551615"}`, same: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var hashes [2][sha256.Size]byte
			for index, body := range []string{testCase.first, testCase.next} {
				// 1. 从认证、路径、幂等头和 JSON DTO 取得实际重命名参数。
				var logs bytes.Buffer
				router := NewRouter(logging.New(&logs), nil, func(context.Context) bool { return true })
				router.PATCH("/test-rename-hash/:id", middleware.RequireAuthorization(func(context.Context, string) (*domain.Actor, error) {
					return &domain.Actor{UserID: 7, DeviceID: 13, SessionID: 17}, nil
				}, handler.AuthorizationFailure), func(requestContext *gin.Context) {
					path, fieldError := dto.ParseEntryPath(requestContext.Param("id"))
					if fieldError != nil {
						handler.InvalidArgument(requestContext, fieldError)
						return
					}
					if _, fieldError := dto.ParseIdempotencyKey(requestContext.Request); fieldError != nil {
						handler.InvalidArgument(requestContext, fieldError)
						return
					}
					var request dto.RenameEntryRequest
					if !binding.ReadJSON(requestContext, &request) {
						return
					}
					input, fieldError := dto.ParseRenameEntryRequest(request)
					if fieldError != nil {
						handler.InvalidArgument(requestContext, fieldError)
						return
					}
					hash, err := service.BuildRenameEntryRequestHash(path.EntryID, input.ExpectedRevision, input.Name)
					if err != nil {
						t.Error("有效请求摘要生成失败", err)
						requestContext.AbortWithStatus(500)
						return
					}
					hashes[index] = hash
					requestContext.AbortWithStatus(204)
				})
				ids := []string{testCase.firstID, testCase.nextID}
				request := httptest.NewRequest("PATCH", "/test-rename-hash/"+ids[index]+"?name=private-query", strings.NewReader(body))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Authorization", "Bearer fixture-access")
				keys := []string{"Request-A", "Request-B"}
				requestIDs := []string{"28ee758a-0727-42de-ae7d-3bb33155e148", "28ee758a-0727-42de-ae7d-3bb33155e149"}
				request.Header.Set("Idempotency-Key", keys[index])
				request.Header.Set("X-Request-ID", requestIDs[index])
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				if response.Code != 204 || response.Body.Len() != 0 || hashes[index] == [sha256.Size]byte{} || response.Header().Get("X-Request-ID") != requestIDs[index] {
					t.Fatal("重命名参数绑定或摘要错误", response.Code)
				}
				if strings.Contains(logs.String(), "private-query") || strings.Contains(logs.String(), keys[index]) || strings.Contains(logs.String(), "学习资料") || strings.Contains(logs.String(), "fixture-access") {
					t.Fatal("测试入口日志包含请求私有数据")
				}
			}
			// 2. 比较两个有效请求，核对展示值和完整整数对应的摘要关系。
			if (hashes[0] == hashes[1]) != testCase.same {
				t.Fatal("请求参数对应的摘要关系错误")
			}
		})
	}
}
