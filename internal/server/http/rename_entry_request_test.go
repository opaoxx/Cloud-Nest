package http

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestRenameEntryRequestHTTP(t *testing.T) {
	// 1. 临时 PATCH 入口组合认证、路径、幂等头、JSON 与重命名 DTO。
	actor := domain.Actor{UserID: 7, DeviceID: 13, SessionID: 17}
	type acceptedRequest struct {
		entryID uint64
		input   *dto.RenameEntryInput
	}
	accepted := make(chan acceptedRequest, 1)
	var logs bytes.Buffer
	router := NewRouter(logging.New(&logs), nil, func(context.Context) bool { return true })
	router.PATCH("/test-rename-entry/:id", middleware.RequireAuthorization(func(_ context.Context, token string) (*domain.Actor, error) {
		if token != "fixture-access" {
			return nil, domain.ErrInvalidAccessToken
		}
		return &actor, nil
	}, handler.AuthorizationFailure), func(requestContext *gin.Context) {
		requestContext.Header("Cache-Control", "no-store")
		identity, exists := requestcontext.Actor(requestContext.Request.Context())
		if !exists || identity != actor {
			t.Error("重命名测试入口身份错误")
		}
		path, fieldError := dto.ParseEntryPath(requestContext.Param("id"))
		if fieldError != nil {
			handler.InvalidArgument(requestContext, fieldError)
			return
		}
		key, fieldError := dto.ParseIdempotencyKey(requestContext.Request)
		if fieldError != nil {
			handler.InvalidArgument(requestContext, fieldError)
			return
		}
		if key != "Request-007" {
			t.Error("解析后的幂等键错误")
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
		accepted <- acceptedRequest{entryID: path.EntryID, input: input}
		requestContext.AbortWithStatus(http.StatusNoContent)
	})
	server := httptest.NewServer(router)
	defer server.Close()
	client := server.Client()
	client.Timeout = 2 * time.Second
	for _, testCase := range []struct {
		name, body, field string
		status            int
		version           uint64
	}{
		{name: "normal", body: `{"name":"新名称 🌱","expected_revision":"3"}`, status: 204, version: 3},
		{name: "reordered fields", body: `{"expected_revision":"3","name":"新名称 🌱"}`, status: 204, version: 3},
		{name: "maximum ids and revision", body: `{"name":"新名称 🌱","expected_revision":"18446744073709551615"}`, status: 204, version: ^uint64(0)},
		{name: "escaped name", body: `{"name":"Cafe\u0301","expected_revision":"1"}`, status: 204, version: 1},
		{name: "missing name", body: `{"expected_revision":"1"}`, status: 400, field: "name"},
		{name: "null name", body: `{"name":null,"expected_revision":"1"}`, status: 400, field: "name"},
		{name: "empty name first", body: `{"name":"","expected_revision":"0"}`, status: 400, field: "name"},
		{name: "missing revision", body: `{"name":"新名称"}`, status: 400, field: "expected_revision"},
		{name: "null revision", body: `{"name":"新名称","expected_revision":null}`, status: 400, field: "expected_revision"},
		{name: "zero revision", body: `{"name":"新名称","expected_revision":"0"}`, status: 400, field: "expected_revision"},
		{name: "leading zero", body: `{"name":"新名称","expected_revision":"01"}`, status: 400, field: "expected_revision"},
		{name: "overflow", body: `{"name":"新名称","expected_revision":"18446744073709551616"}`, status: 400, field: "expected_revision"},
		{name: "numeric revision", body: `{"name":"新名称","expected_revision":3}`, status: 400},
		{name: "boolean revision", body: `{"name":"新名称","expected_revision":true}`, status: 400},
		{name: "numeric name", body: `{"name":3,"expected_revision":"1"}`, status: 400},
		{name: "unknown user", body: `{"name":"新名称","expected_revision":"1","user_id":"private-user"}`, status: 400},
		{name: "unknown path id", body: `{"name":"新名称","expected_revision":"1","id":"23"}`, status: 400},
		{name: "unknown color", body: `{"name":"新名称","expected_revision":"1","color_key":"private-color"}`, status: 400},
		{name: "json null", body: `null`, status: 400},
		{name: "json array", body: `[]`, status: 400},
		{name: "two objects", body: `{"name":"新名称","expected_revision":"1"} {}`, status: 400},
		{name: "invalid utf8", body: "{\"name\":\"private-name\xff\",\"expected_revision\":\"1\"}", status: 400},
		{name: "body size", body: strings.Repeat(" ", (1<<20)+1), status: 413},
		{name: "media type", body: `{}`, status: 415},
		{name: "path first", body: `{"private-broken":`, status: 400, field: "id"},
		{name: "header first", body: `{"private-broken":`, status: 400, field: "Idempotency-Key"},
		{name: "duplicate key", body: `{}`, status: 400, field: "Idempotency-Key"},
		{name: "authorization first", body: `{"private-broken":`, status: 401},
		{name: "invalid authorization", body: `{"private-broken":`, status: 401},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			path := "23"
			if testCase.name == "path first" {
				path = "0"
			}
			if testCase.name == "maximum ids and revision" {
				path = "18446744073709551615"
			}
			message, err := http.NewRequest("PATCH", server.URL+"/test-rename-entry/"+path+"?note=private-query", strings.NewReader(testCase.body))
			if err != nil {
				t.Fatal(err)
			}
			message.Header.Set("Content-Type", "application/json")
			message.Header.Set("Authorization", "Bearer fixture-access")
			message.Header.Set("Idempotency-Key", " Request-007 ")
			switch testCase.name {
			case "media type":
				message.Header.Set("Content-Type", "text/plain")
			case "header first":
				message.Header.Del("Idempotency-Key")
			case "duplicate key":
				message.Header.Add("Idempotency-Key", "Request-007")
			case "authorization first":
				message.Header.Del("Authorization")
				message.Header.Del("Idempotency-Key")
			case "invalid authorization":
				message.Header.Set("Authorization", "Bearer private-access")
				message.Header.Del("Idempotency-Key")
			}
			response, err := client.Do(message)
			if err != nil {
				t.Fatal("重命名正文 HTTP 请求失败", err)
			}
			body, readError := io.ReadAll(response.Body)
			closeError := response.Body.Close()
			if readError != nil || closeError != nil || response.StatusCode != testCase.status || response.Header.Get("Cache-Control") != "no-store" {
				t.Fatal("重命名正文实际传输状态错误", response.StatusCode, readError, closeError)
			}

			// 2. 核对成功捕获的参数或错误字段，错误路径保持空输入。
			if testCase.status == 204 {
				select {
				case received := <-accepted:
					wantID := uint64(23)
					wantName := "新名称 🌱"
					if testCase.name == "maximum ids and revision" {
						wantID = ^uint64(0)
					}
					if testCase.name == "escaped name" {
						wantName = "Cafe\u0301"
					}
					if received.entryID != wantID || received.input.ExpectedRevision != testCase.version || received.input.Name != wantName || len(body) != 0 {
						t.Fatal("捕获的目标、名称或版本错误")
					}
				default:
					t.Fatal("成功请求未产生结构化输入")
				}
			} else {
				var failure dto.ErrorResponse
				code := "INVALID_ARGUMENT"
				switch testCase.status {
				case 401:
					code = "AUTH_REQUIRED"
				case 413:
					code = "REQUEST_BODY_TOO_LARGE"
				case 415:
					code = "UNSUPPORTED_MEDIA_TYPE"
				}
				if json.Unmarshal(body, &failure) != nil || failure.Error.Code != code || failure.RequestID == "" || failure.RequestID != response.Header.Get("X-Request-ID") || len(accepted) != 0 {
					t.Fatal("正文错误结构、关联或输入状态错误")
				}
				if testCase.field == "" {
					if failure.Error.Details != nil {
						t.Fatal("普通绑定错误 details 非空")
					}
				} else if len(failure.Error.Details) != 1 || failure.Error.Details["field"] != testCase.field {
					t.Fatal("正文错误定位不符")
				}
				if bytes.Contains(body, []byte("private-")) {
					t.Fatal("错误回显原始输入")
				}
			}
		})
	}
	server.Close()
	for _, marker := range []string{"private-query", "private-access", "private-user", "private-color", "private-name", "Request-007", "新名称"} {
		if strings.Contains(logs.String(), marker) {
			t.Fatal("完成日志包含请求原文")
		}
	}
}

func TestRenameEntryRequestNameRules(t *testing.T) {
	// 1. DTO 保留原文，后续服务继续使用既有名称和判重键规则。
	for _, testCase := range []struct {
		raw, display, key string
		valid             bool
	}{
		{raw: "  Cafe\u0301  ", display: "Cafe\u0301", key: "café", valid: true},
		{raw: "资料 🌱", display: "资料 🌱", key: "资料 🌱", valid: true},
		{raw: "   "}, {raw: "CON.txt"}, {raw: "a/b"}, {raw: "private\nname"}, {raw: strings.Repeat("a", 121)},
	} {
		input, fieldError := dto.ParseRenameEntryRequest(dto.RenameEntryRequest{Name: testCase.raw, ExpectedRevision: "3"})
		if fieldError != nil || input.Name != testCase.raw {
			t.Fatal("DTO 提前改变名称或丢失原文", fieldError)
		}
		display, err := service.ValidateEntryName(input.Name)
		if testCase.valid {
			key, keyError := service.BuildEntryNameKey(input.Name)
			if err != nil || keyError != nil || display != testCase.display || string(key) != testCase.key {
				t.Fatal("名称及判重键衔接错误", err, keyError)
			}
		} else if err != domain.ErrInvalidEntryName {
			t.Fatal("完整命名规则未拒绝无效名称", err)
		}
	}
}
