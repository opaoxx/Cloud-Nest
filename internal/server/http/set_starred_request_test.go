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

func TestSetStarredRequestHTTP(t *testing.T) {
	// 1. 测试 PUT 入口连接认证、源路径、JSON 读取和收藏 DTO。
	actor := domain.Actor{UserID: 7, DeviceID: 13, SessionID: 17}
	type acceptedRequest struct {
		actor   domain.Actor
		entryID uint64
		input   *dto.SetStarredInput
	}
	accepted := make(chan acceptedRequest, 1)
	router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
	router.PUT("/test-entry-star/:id", middleware.RequireAuthorization(func(_ context.Context, token string) (*domain.Actor, error) {
		if token != "fixture-access" {
			return nil, domain.ErrInvalidAccessToken
		}
		return &actor, nil
	}, handler.AuthorizationFailure), func(requestContext *gin.Context) {
		requestContext.Header("Cache-Control", "no-store")
		identity, exists := requestcontext.Actor(requestContext.Request.Context())
		if !exists || identity != actor {
			t.Error("收藏测试入口身份错误")
		}
		path, fieldError := dto.ParseEntryPath(requestContext.Param("id"))
		if fieldError != nil {
			handler.InvalidArgument(requestContext, fieldError)
			return
		}
		var request dto.SetStarredRequest
		if !binding.ReadJSON(requestContext, &request) {
			return
		}
		input, fieldError := dto.ParseSetStarredRequest(request)
		if fieldError != nil {
			handler.InvalidArgument(requestContext, fieldError)
			return
		}
		accepted <- acceptedRequest{actor: identity, entryID: path.EntryID, input: input}
		requestContext.AbortWithStatus(http.StatusNoContent)
	})
	server := httptest.NewServer(router)
	defer server.Close()
	client := server.Client()
	client.Timeout = 2 * time.Second
	for _, testCase := range []struct {
		name, body, id, field, code string
		status                      int
		starred                     bool
	}{
		{name: "star", body: `{"starred":true}`, status: 204, starred: true},
		{name: "unstar", body: `{"starred":false}`, status: 204},
		{name: "formatted false", body: "{\n  \"starred\": false\n}\n", status: 204},
		{name: "escaped field", body: `{"\u0073tarred":true}`, status: 204, starred: true},
		{name: "case matching", body: `{"STARRED":false}`, status: 204},
		{name: "last boolean wins", body: `{"starred":true,"starred":false}`, status: 204},
		{name: "null then false", body: `{"starred":null,"starred":false}`, status: 204},
		{name: "with idempotency header", body: `{"starred":false}`, status: 204},
		{name: "maximum id", id: "18446744073709551615", body: `{"starred":true}`, status: 204, starred: true},
		{name: "high precision id", id: "9007199254740993", body: `{"starred":false}`, status: 204},
		{name: "missing state", body: `{}`, status: 400, field: "starred"},
		{name: "null state", body: `{"starred":null}`, status: 400, field: "starred"},
		{name: "boolean then null", body: `{"starred":false,"starred":null}`, status: 400, field: "starred"},
		{name: "string state", body: `{"starred":"false"}`, status: 400},
		{name: "numeric state", body: `{"starred":0}`, status: 400},
		{name: "array state", body: `{"starred":[]}`, status: 400},
		{name: "object state", body: `{"starred":{}}`, status: 400},
		{name: "uppercase boolean", body: `{"starred":FALSE}`, status: 400},
		{name: "unknown field", body: `{"starred":true,"user_id":"99"}`, status: 400},
		{name: "revision field", body: `{"starred":true,"expected_revision":"3"}`, status: 400},
		{name: "empty body", status: 400},
		{name: "null body", body: `null`, status: 400},
		{name: "array body", body: `[false]`, status: 400},
		{name: "scalar body", body: `false`, status: 400},
		{name: "malformed json", body: `{"private-broken":`, status: 400},
		{name: "two objects", body: `{"starred":true} {"starred":false}`, status: 400},
		{name: "invalid path first", id: "0", body: `{"private-broken":`, status: 400, field: "id"},
		{name: "overflow path", id: "18446744073709551616", body: `{"starred":false}`, status: 400, field: "id"},
		{name: "noncanonical path", id: "023", body: `{"starred":false}`, status: 400, field: "id"},
		{name: "missing authorization", id: "0", body: `{"private-broken":`, status: 401, code: "AUTH_REQUIRED"},
		{name: "rejected authorization", body: `{"starred":true}`, status: 401, code: "AUTH_REQUIRED"},
		{name: "unsupported type", body: `{"starred":true}`, status: 415, code: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "missing type", body: `{"starred":true}`, status: 415, code: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "compressed body", body: `{"starred":true}`, status: 415, code: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "oversized body", body: `{"starred":false}`, status: 413, code: "REQUEST_BODY_TOO_LARGE"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 2. 通过真实 HTTP 请求核对布尔语义与各层错误定位。
			id := testCase.id
			if id == "" {
				id = "23"
			}
			body := testCase.body
			if testCase.name == "oversized body" {
				body = strings.Repeat(" ", 1<<20) + body
			}
			message, err := http.NewRequest("PUT", server.URL+"/test-entry-star/"+id, strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			message.Header.Set("Content-Type", "application/json")
			if testCase.name != "missing authorization" {
				token := "fixture-access"
				if testCase.name == "rejected authorization" {
					token = "rejected-access"
				}
				message.Header.Set("Authorization", "Bearer "+token)
			}
			switch testCase.name {
			case "unsupported type":
				message.Header.Set("Content-Type", "text/plain")
			case "missing type":
				message.Header.Del("Content-Type")
			case "compressed body":
				message.Header.Set("Content-Encoding", "gzip")
			case "with idempotency header":
				message.Header.Set("Idempotency-Key", "Request-007")
			}
			response, err := client.Do(message)
			if err != nil {
				t.Fatal(err)
			}
			encoded, readError := io.ReadAll(response.Body)
			closeError := response.Body.Close()
			if readError != nil || closeError != nil || response.StatusCode != testCase.status || response.Header.Get("X-Request-ID") == "" || response.Header.Get("Cache-Control") != "no-store" {
				t.Fatal("收藏请求状态或响应头错误", response.StatusCode, readError, closeError)
			}
			if testCase.status == 204 {
				received := <-accepted
				if received.actor != actor || strconv.FormatUint(received.entryID, 10) != id || received.input == nil || received.input.Starred != testCase.starred || len(encoded) != 0 {
					t.Fatal("收藏状态或路径标识传递错误")
				}
				return
			}
			var failure dto.ErrorResponse
			code := testCase.code
			if code == "" {
				code = "INVALID_ARGUMENT"
			}
			if json.Unmarshal(encoded, &failure) != nil || failure.Error.Code != code || failure.RequestID != response.Header.Get("X-Request-ID") {
				t.Fatal("收藏错误响应不符")
			}
			if testCase.field != "" {
				if len(failure.Error.Details) != 1 || failure.Error.Details["field"] != testCase.field {
					t.Fatal("收藏错误字段定位不符")
				}
			} else if failure.Error.Details != nil {
				t.Fatal("正文或认证错误详情不符")
			}
			select {
			case <-accepted:
				t.Fatal("错误请求进入了参数接收分支")
			default:
			}
		})
	}
}
