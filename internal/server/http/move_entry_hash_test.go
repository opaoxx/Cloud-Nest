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

func TestMoveEntryRequestHashHTTPFlow(t *testing.T) {
	for _, testCase := range []struct {
		name, firstID, nextID, first, next string
		same                               bool
	}{
		{name: "field order and whitespace", firstID: "13", nextID: "13",
			first: `{"target_parent_id":"14","expected_revision":"3"}`,
			next:  "{\n  \"expected_revision\":\"3\", \"target_parent_id\":\"14\"\n}", same: true},
		{name: "escaped digits", firstID: "13", nextID: "13",
			first: `{"target_parent_id":"14","expected_revision":"3"}`,
			next:  `{"target_parent_id":"\u0031\u0034","expected_revision":"\u0033"}`, same: true},
		{name: "escaped field name", firstID: "13", nextID: "13",
			first: `{"target_parent_id":"14","expected_revision":"3"}`,
			next:  `{"target_parent_\u0069d":"14","expected_revision":"3"}`, same: true},
		{name: "source", firstID: "13", nextID: "15",
			first: `{"target_parent_id":"14","expected_revision":"3"}`,
			next:  `{"target_parent_id":"14","expected_revision":"3"}`},
		{name: "revision", firstID: "13", nextID: "13",
			first: `{"target_parent_id":"14","expected_revision":"3"}`,
			next:  `{"target_parent_id":"14","expected_revision":"4"}`},
		{name: "destination", firstID: "13", nextID: "13",
			first: `{"target_parent_id":"14","expected_revision":"3"}`,
			next:  `{"target_parent_id":"15","expected_revision":"3"}`},
		{name: "high source precision", firstID: "9007199254740992", nextID: "9007199254740993",
			first: `{"target_parent_id":"14","expected_revision":"3"}`,
			next:  `{"target_parent_id":"14","expected_revision":"3"}`},
		{name: "high revision precision", firstID: "13", nextID: "13",
			first: `{"target_parent_id":"14","expected_revision":"9007199254740992"}`,
			next:  `{"target_parent_id":"14","expected_revision":"9007199254740993"}`},
		{name: "high destination precision", firstID: "13", nextID: "13",
			first: `{"target_parent_id":"9007199254740992","expected_revision":"3"}`,
			next:  `{"target_parent_id":"9007199254740993","expected_revision":"3"}`},
		{name: "maximum values", firstID: "18446744073709551615", nextID: "18446744073709551615",
			first: `{"target_parent_id":"18446744073709551615","expected_revision":"18446744073709551615"}`,
			next:  `{"target_parent_id":"18446744073709551615","expected_revision":"18446744073709551615"}`, same: true},
		{name: "minimum values", firstID: "1", nextID: "1",
			first: `{"target_parent_id":"1","expected_revision":"1"}`,
			next:  `{"target_parent_id":"1","expected_revision":"1"}`, same: true},
		{name: "field boundaries", firstID: "1", nextID: "12",
			first: `{"target_parent_id":"4","expected_revision":"23"}`,
			next:  `{"target_parent_id":"4","expected_revision":"3"}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var hashes [2][sha256.Size]byte
			for index, body := range []string{testCase.first, testCase.next} {
				// 1. 从认证、路径、幂等头和 JSON DTO 取得实际移动参数。
				var logs bytes.Buffer
				router := NewRouter(logging.New(&logs), nil, func(context.Context) bool { return true })
				router.POST("/test-move-hash/:id", middleware.RequireAuthorization(func(context.Context, string) (*domain.Actor, error) {
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
					var request dto.MoveEntryRequest
					if !binding.ReadJSON(requestContext, &request) {
						return
					}
					input, fieldError := dto.ParseMoveEntryRequest(request)
					if fieldError != nil {
						handler.InvalidArgument(requestContext, fieldError)
						return
					}
					hash, err := service.BuildMoveEntryRequestHash(path.EntryID, input.ExpectedRevision, input.TargetParentID)
					if err != nil {
						t.Error("有效请求摘要生成失败", err)
						requestContext.AbortWithStatus(500)
						return
					}
					hashes[index] = hash
					requestContext.AbortWithStatus(204)
				})
				ids := []string{testCase.firstID, testCase.nextID}
				request := httptest.NewRequest("POST", "/test-move-hash/"+ids[index]+"?target_parent_id=private-query", strings.NewReader(body))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Authorization", "Bearer fixture-access")
				keys := []string{"Request-A", "Request-B"}
				requestIDs := []string{"28ee758a-0727-42de-ae7d-3bb33155e148", "28ee758a-0727-42de-ae7d-3bb33155e149"}
				request.Header.Set("Idempotency-Key", keys[index])
				request.Header.Set("X-Request-ID", requestIDs[index])
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				if response.Code != 204 || response.Body.Len() != 0 || hashes[index] == [sha256.Size]byte{} || response.Header().Get("X-Request-ID") != requestIDs[index] {
					t.Fatal("移动参数绑定或摘要错误", response.Code)
				}
				if strings.Contains(logs.String(), "private-query") || strings.Contains(logs.String(), keys[index]) || strings.Contains(logs.String(), "target_parent_id") || strings.Contains(logs.String(), "fixture-access") {
					t.Fatal("测试入口日志包含请求私有数据")
				}
			}
			// 2. 比较两个有效请求，核对完整整数对应的摘要关系。
			if (hashes[0] == hashes[1]) != testCase.same {
				t.Fatal("请求参数对应的摘要关系错误")
			}
		})
	}
}
