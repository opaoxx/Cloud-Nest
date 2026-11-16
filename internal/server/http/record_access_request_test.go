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

func TestRecordAccessRequestHTTP(t *testing.T) {
	// 1. 测试 POST 入口组合认证、路径、JSON 读取和访问用途 DTO。
	actor := domain.Actor{UserID: 7, DeviceID: 13, SessionID: 17}
	type acceptedRequest struct {
		actor   domain.Actor
		entryID uint64
		input   *dto.RecordAccessInput
	}
	accepted := make(chan acceptedRequest, 1)
	router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
	router.POST("/test-entry-access/:id", middleware.RequireAuthorization(func(_ context.Context, token string) (*domain.Actor, error) {
		if token != "fixture-access" {
			return nil, domain.ErrInvalidAccessToken
		}
		return &actor, nil
	}, handler.AuthorizationFailure), func(requestContext *gin.Context) {
		requestContext.Header("Cache-Control", "no-store")
		identity, exists := requestcontext.Actor(requestContext.Request.Context())
		if !exists || identity != actor {
			t.Error("访问测试入口身份错误")
			handler.InternalError(requestContext, nil)
			return
		}
		path, fieldError := dto.ParseEntryPath(requestContext.Param("id"))
		if fieldError != nil {
			handler.InvalidArgument(requestContext, fieldError)
			return
		}
		var request dto.RecordAccessRequest
		if !binding.ReadJSON(requestContext, &request) {
			return
		}
		input, fieldError := dto.ParseRecordAccessRequest(request)
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
		name, body, id, purpose, field, code string
		status                               int
	}{
		{name: "preview", body: `{"purpose":"preview"}`, status: 204, purpose: "preview"},
		{name: "open", body: `{"purpose":"open"}`, status: 204, purpose: "open"},
		{name: "formatted open", body: "{\n  \"purpose\": \"open\"\n}\n", status: 204, purpose: "open"},
		{name: "escaped field", body: `{"\u0070urpose":"preview"}`, status: 204, purpose: "preview"},
		{name: "case matching", body: `{"PURPOSE":"open"}`, status: 204, purpose: "open"},
		{name: "last string wins", body: `{"purpose":"preview","purpose":"open"}`, status: 204, purpose: "open"},
		{name: "null then preview", body: `{"purpose":null,"purpose":"preview"}`, status: 204, purpose: "preview"},
		{name: "with idempotency header", body: `{"purpose":"open"}`, status: 204, purpose: "open"},
		{name: "maximum id", id: "18446744073709551615", body: `{"purpose":"open"}`, status: 204, purpose: "open"},
		{name: "high precision id", id: "9007199254740993", body: `{"purpose":"preview"}`, status: 204, purpose: "preview"},
		{name: "missing purpose", body: `{}`, status: 400, field: "purpose"},
		{name: "null purpose", body: `{"purpose":null}`, status: 400, field: "purpose"},
		{name: "string then null", body: `{"purpose":"preview","purpose":null}`, status: 400, field: "purpose"},
		{name: "uppercase value", body: `{"purpose":"PREVIEW"}`, status: 400, field: "purpose"},
		{name: "padded value", body: `{"purpose":" open "}`, status: 400, field: "purpose"},
		{name: "download purpose", body: `{"purpose":"download"}`, status: 400, field: "purpose"},
		{name: "empty purpose", body: `{"purpose":""}`, status: 400, field: "purpose"},
		{name: "numeric purpose", body: `{"purpose":1}`, status: 400},
		{name: "boolean purpose", body: `{"purpose":true}`, status: 400},
		{name: "array purpose", body: `{"purpose":[]}`, status: 400},
		{name: "object purpose", body: `{"purpose":{}}`, status: 400},
		{name: "unknown owner", body: `{"purpose":"open","user_id":"99"}`, status: 400},
		{name: "client time", body: `{"purpose":"open","last_opened_at":"2026-11-16T00:00:00Z"}`, status: 400},
		{name: "revision field", body: `{"purpose":"open","expected_revision":"3"}`, status: 400},
		{name: "empty body", status: 400},
		{name: "null body", body: `null`, status: 400},
		{name: "array body", body: `[]`, status: 400},
		{name: "scalar body", body: `"open"`, status: 400},
		{name: "malformed json", body: `{"private-broken":`, status: 400},
		{name: "two objects", body: `{"purpose":"open"} {"purpose":"preview"}`, status: 400},
		{name: "invalid path first", id: "0", body: `{"private-broken":`, status: 400, field: "id"},
		{name: "overflow path", id: "18446744073709551616", body: `{"purpose":"open"}`, status: 400, field: "id"},
		{name: "noncanonical path", id: "023", body: `{"purpose":"open"}`, status: 400, field: "id"},
		{name: "missing authorization", id: "0", body: `{"private-broken":`, status: 401, code: "AUTH_REQUIRED"},
		{name: "rejected authorization", body: `{"purpose":"open"}`, status: 401, code: "AUTH_REQUIRED"},
		{name: "unsupported type", body: `{"purpose":"open"}`, status: 415, code: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "missing type", body: `{"purpose":"open"}`, status: 415, code: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "compressed body", body: `{"purpose":"open"}`, status: 415, code: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "oversized body", body: `{"purpose":"open"}`, status: 413, code: "REQUEST_BODY_TOO_LARGE"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 2. 通过真实 HTTP 请求核对合法用途、完整标识和输入错误顺序。
			id := testCase.id
			if id == "" {
				id = "23"
			}
			body := testCase.body
			if testCase.name == "oversized body" {
				body = strings.Repeat(" ", 1<<20) + body
			}
			message, err := http.NewRequest("POST", server.URL+"/test-entry-access/"+id+"?user_id=99&purpose=private-query", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			message.Header.Set("Content-Type", "application/json")
			message.Header.Set("X-User-ID", "99")
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
				t.Fatal("访问用途请求状态或响应头错误", response.StatusCode, readError, closeError)
			}
			if testCase.status == 204 {
				received := <-accepted
				if received.actor != actor || strconv.FormatUint(received.entryID, 10) != id || received.input == nil || received.input.Purpose != testCase.purpose || len(encoded) != 0 {
					t.Fatal("访问用途或完整目标传递错误")
				}
				return
			}
			var failure dto.ErrorResponse
			code := testCase.code
			if code == "" {
				code = "INVALID_ARGUMENT"
			}
			if json.Unmarshal(encoded, &failure) != nil || failure.Error.Code != code || failure.RequestID != response.Header.Get("X-Request-ID") {
				t.Fatal("访问用途错误响应不符")
			}
			if testCase.field != "" {
				if len(failure.Error.Details) != 1 || failure.Error.Details["field"] != testCase.field {
					t.Fatal("访问用途字段错误位置不符")
				}
			} else if failure.Error.Details != nil {
				t.Fatal("正文或认证错误详情不符")
			}
			if strings.Contains(string(encoded), "private-broken") || strings.Contains(string(encoded), "private-query") {
				t.Fatal("错误响应包含原始请求值")
			}
			select {
			case <-accepted:
				t.Fatal("错误请求进入参数接收分支")
			default:
			}
		})
	}
}
