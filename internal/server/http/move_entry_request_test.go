package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

func TestMoveEntryRequestHTTP(t *testing.T) {
	// 1. 测试 POST 入口连接认证、源路径、幂等头和移动 DTO。
	actor := domain.Actor{UserID: 7, DeviceID: 13, SessionID: 17}
	type acceptedRequest struct {
		entryID uint64
		input   *dto.MoveEntryInput
	}
	accepted := make(chan acceptedRequest, 1)
	router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
	router.POST("/test-move-entry/:id", middleware.RequireAuthorization(func(_ context.Context, token string) (*domain.Actor, error) {
		if token != "fixture-access" {
			return nil, domain.ErrInvalidAccessToken
		}
		return &actor, nil
	}, handler.AuthorizationFailure), func(requestContext *gin.Context) {
		requestContext.Header("Cache-Control", "no-store")
		identity, exists := requestcontext.Actor(requestContext.Request.Context())
		if !exists || identity != actor {
			t.Error("移动测试入口身份错误")
		}
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
		accepted <- acceptedRequest{entryID: path.EntryID, input: input}
		requestContext.AbortWithStatus(http.StatusNoContent)
	})
	server := httptest.NewServer(router)
	defer server.Close()
	client := server.Client()
	client.Timeout = 2 * time.Second
	for _, testCase := range []struct {
		name, body, id, field string
		status                int
		parent, revision      uint64
	}{
		{name: "normal", body: `{"target_parent_id":"12","expected_revision":"3"}`, status: 204, parent: 12, revision: 3},
		{name: "reordered", body: `{"expected_revision":"3","target_parent_id":"12"}`, status: 204, parent: 12, revision: 3},
		{name: "maximum integers", id: "18446744073709551615", body: `{"target_parent_id":"18446744073709551615","expected_revision":"18446744073709551615"}`, status: 204, parent: ^uint64(0), revision: ^uint64(0)},
		{name: "high precision", body: `{"target_parent_id":"9007199254740993","expected_revision":"9223372036854775808"}`, status: 204, parent: 9007199254740993, revision: 9223372036854775808},
		{name: "root destination id", body: `{"target_parent_id":"11","expected_revision":"1"}`, status: 204, parent: 11, revision: 1},
		{name: "relationship deferred", body: `{"target_parent_id":"23","expected_revision":"3"}`, status: 204, parent: 23, revision: 3},
		{name: "missing parent", body: `{"expected_revision":"3"}`, status: 400, field: "target_parent_id"},
		{name: "null parent", body: `{"target_parent_id":null,"expected_revision":"3"}`, status: 400, field: "target_parent_id"},
		{name: "zero parent", body: `{"target_parent_id":"0","expected_revision":"3"}`, status: 400, field: "target_parent_id"},
		{name: "padded parent", body: `{"target_parent_id":" 12 ","expected_revision":"3"}`, status: 400, field: "target_parent_id"},
		{name: "parent leading zero", body: `{"target_parent_id":"012","expected_revision":"3"}`, status: 400, field: "target_parent_id"},
		{name: "parent overflow", body: `{"target_parent_id":"18446744073709551616","expected_revision":"3"}`, status: 400, field: "target_parent_id"},
		{name: "numeric parent", body: `{"target_parent_id":12,"expected_revision":"3"}`, status: 400},
		{name: "missing revision", body: `{"target_parent_id":"12"}`, status: 400, field: "expected_revision"},
		{name: "null revision", body: `{"target_parent_id":"12","expected_revision":null}`, status: 400, field: "expected_revision"},
		{name: "zero revision", body: `{"target_parent_id":"12","expected_revision":"0"}`, status: 400, field: "expected_revision"},
		{name: "revision leading zero", body: `{"target_parent_id":"12","expected_revision":"03"}`, status: 400, field: "expected_revision"},
		{name: "revision overflow", body: `{"target_parent_id":"12","expected_revision":"18446744073709551616"}`, status: 400, field: "expected_revision"},
		{name: "numeric revision", body: `{"target_parent_id":"12","expected_revision":3}`, status: 400},
		{name: "both invalid", body: `{"target_parent_id":"0","expected_revision":"0"}`, status: 400, field: "target_parent_id"},
		{name: "null object", body: `null`, status: 400},
		{name: "unknown actor", body: `{"target_parent_id":"12","expected_revision":"3","user_id":"99"}`, status: 400},
		{name: "unknown source", body: `{"target_parent_id":"12","expected_revision":"3","entry_id":"99"}`, status: 400},
		{name: "malformed", body: `{"private-broken":`, status: 400},
		{name: "two objects", body: `{"target_parent_id":"12","expected_revision":"3"} {}`, status: 400},
		{name: "invalid path first", id: "0", body: `{"private-broken":`, status: 400, field: "id"},
		{name: "missing key first", body: `{"private-broken":`, status: 400, field: "Idempotency-Key"},
		{name: "missing authorization first", id: "0", body: `{"private-broken":`, status: 401},
		{name: "rejected authorization", body: `{"private-broken":`, status: 401},
		{name: "unsupported type", body: `{"target_parent_id":"12","expected_revision":"3"}`, status: 415},
		{name: "compressed body", body: `{"target_parent_id":"12","expected_revision":"3"}`, status: 415},
		{name: "oversized", body: `{"target_parent_id":"12","expected_revision":"3"}`, status: 413},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			id := testCase.id
			if id == "" {
				id = "23"
			}
			body := testCase.body
			if testCase.name == "oversized" {
				body = strings.Repeat(" ", 1<<20) + body
			}
			message, err := http.NewRequest("POST", server.URL+"/test-move-entry/"+id, strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			message.Header.Set("Content-Type", "application/json")
			message.Header.Set("Idempotency-Key", "Request-007")
			message.Header.Set("Authorization", "Bearer fixture-access")
			switch testCase.name {
			case "missing key first":
				message.Header.Del("Idempotency-Key")
			case "missing authorization first":
				message.Header.Del("Authorization")
				message.Header.Del("Idempotency-Key")
			case "rejected authorization":
				message.Header.Set("Authorization", "Bearer invalid-access")
			case "unsupported type":
				message.Header.Set("Content-Type", "text/plain")
			case "compressed body":
				message.Header.Set("Content-Encoding", "gzip")
			}
			response, err := client.Do(message)
			if err != nil {
				t.Fatal("移动参数 HTTP 请求失败", err)
			}
			encoded, readError := io.ReadAll(response.Body)
			closeError := response.Body.Close()
			if readError != nil || closeError != nil || response.StatusCode != testCase.status {
				t.Fatal("移动参数响应状态错误", response.StatusCode, readError, closeError)
			}
			// 2. 核对完整整数传递及统一错误定位。
			if testCase.status == 204 {
				select {
				case received := <-accepted:
					if strconv.FormatUint(received.entryID, 10) != id || received.input.TargetParentID != testCase.parent || received.input.ExpectedRevision != testCase.revision || len(encoded) != 0 {
						t.Fatal("移动参数传递错误")
					}
				default:
					t.Fatal("有效参数未到达测试入口")
				}
			} else {
				var failure dto.ErrorResponse
				if json.Unmarshal(encoded, &failure) != nil || failure.RequestID == "" || failure.RequestID != response.Header.Get("X-Request-ID") {
					t.Fatal("移动参数错误响应格式错误")
				}
				expectedCodes := map[int]string{400: "INVALID_ARGUMENT", 401: "AUTH_REQUIRED", 413: "REQUEST_BODY_TOO_LARGE", 415: "UNSUPPORTED_MEDIA_TYPE"}
				if failure.Error.Code != expectedCodes[testCase.status] {
					t.Fatal("移动参数错误码错误", failure.Error.Code)
				}
				if testCase.field != "" && (failure.Error.Code != "INVALID_ARGUMENT" || failure.Error.Details["field"] != testCase.field) {
					t.Fatal("移动参数字段错误", failure.Error)
				}
				if testCase.field == "" && failure.Error.Details != nil {
					t.Fatal("正文或认证错误详情应为 null")
				}
				select {
				case <-accepted:
					t.Fatal("失败参数到达测试入口")
				default:
				}
			}
		})
	}
}
