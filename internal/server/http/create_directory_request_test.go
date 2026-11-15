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
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestCreateDirectoryRequestHTTPFlow(t *testing.T) {
	validBody := `{"parent_id":"11","name":"学习资料"}`
	maximumBody := validBody + strings.Repeat(" ", int(binding.MaxJSONBodyBytes)-len(validBody))
	for _, testCase := range []struct {
		name, body, contentType, encoding, field, code string
		entryName, color                               string
		parentID                                       uint64
		status                                         int
		hasColor, noAuthorization, badToken            bool
	}{
		{name: "required fields", body: validBody, parentID: 11, entryName: "学习资料", status: 204},
		{name: "maximum parent", body: `{"parent_id":"18446744073709551615","name":"资料"}`, parentID: ^uint64(0), entryName: "资料", status: 204},
		{name: "above javascript safe integer", body: `{"parent_id":"9007199254740993","name":"资料"}`, parentID: 9007199254740993, entryName: "资料", status: 204},
		{name: "null color", body: `{"parent_id":"11","name":"资料","color_key":null}`, parentID: 11, entryName: "资料", status: 204},
		{name: "empty color", body: `{"parent_id":"11","name":"资料","color_key":""}`, parentID: 11, entryName: "资料", hasColor: true, status: 204},
		{name: "color and raw name", body: `{"parent_id":"11","name":"  Cafe\u0301 🌱  ","color_key":"green"}`, parentID: 11, entryName: "  Cafe\u0301 🌱  ", hasColor: true, color: "green", status: 204},
		{name: "maximum color", body: `{"parent_id":"11","name":"资料","color_key":"abcdefghijklmnop"}`, parentID: 11, entryName: "资料", hasColor: true, color: "abcdefghijklmnop", status: 204},
		{name: "business name check follows DTO", body: `{"parent_id":"11","name":"资料/一"}`, parentID: 11, entryName: "资料/一", status: 204},
		{name: "maximum body", body: maximumBody, parentID: 11, entryName: "学习资料", status: 204},
		{name: "missing parent", body: `{"name":"资料"}`, status: 400, field: "parent_id"},
		{name: "null parent", body: `{"parent_id":null,"name":"资料"}`, status: 400, field: "parent_id"},
		{name: "zero parent", body: `{"parent_id":"0","name":"资料"}`, status: 400, field: "parent_id"},
		{name: "leading zero parent", body: `{"parent_id":"011","name":"资料"}`, status: 400, field: "parent_id"},
		{name: "overflow parent", body: `{"parent_id":"18446744073709551616","name":"资料"}`, status: 400, field: "parent_id"},
		{name: "numeric parent", body: `{"parent_id":11,"name":"资料"}`, status: 400},
		{name: "missing name", body: `{"parent_id":"11"}`, status: 400, field: "name"},
		{name: "null name", body: `{"parent_id":"11","name":null}`, status: 400, field: "name"},
		{name: "empty name", body: `{"parent_id":"11","name":""}`, status: 400, field: "name"},
		{name: "object name", body: `{"parent_id":"11","name":{}}`, status: 400},
		{name: "long color", body: `{"parent_id":"11","name":"资料","color_key":"abcdefghijklmnopq"}`, status: 400, field: "color_key"},
		{name: "unicode color", body: `{"parent_id":"11","name":"资料","color_key":"绿色"}`, status: 400, field: "color_key"},
		{name: "control color", body: `{"parent_id":"11","name":"资料","color_key":"green\n"}`, status: 400, field: "color_key"},
		{name: "numeric color", body: `{"parent_id":"11","name":"资料","color_key":1}`, status: 400},
		{name: "boolean color", body: `{"parent_id":"11","name":"资料","color_key":false}`, status: 400},
		{name: "array color", body: `{"parent_id":"11","name":"资料","color_key":[]}`, status: 400},
		{name: "client supplied owner", body: `{"parent_id":"11","name":"资料","user_id":"99"}`, status: 400},
		{name: "client supplied name key", body: `{"parent_id":"11","name":"资料","name_key":"private-key"}`, status: 400},
		{name: "client supplied entry type", body: `{"parent_id":"11","name":"资料","entry_type":"FILE"}`, status: 400},
		{name: "invalid utf8 body", body: "{\"parent_id\":\"11\",\"name\":\"private-name\xff\"}", status: 400},
		{name: "invalid json", body: `{"parent_id":"private-parent",`, status: 400},
		{name: "multiple objects", body: validBody + `{}`, status: 400},
		{name: "array body", body: `[]`, status: 400},
		{name: "oversized body", body: maximumBody + " ", status: 413, code: "REQUEST_BODY_TOO_LARGE"},
		{name: "unsupported media type", body: validBody, contentType: "text/plain", status: 415, code: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "unsupported charset", body: validBody, contentType: "application/json; charset=gbk", status: 415, code: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "compressed body", body: validBody, encoding: "gzip", status: 415, code: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "missing bearer before body", body: `{"private-broken":`, noAuthorization: true, status: 401, code: "AUTH_REQUIRED"},
		{name: "rejected bearer before body", body: `{"private-broken":`, badToken: true, status: 401, code: "SESSION_REVOKED"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 测试入口组合认证、既有 JSON 读取器和本轮 DTO 解析。
			var logs bytes.Buffer
			var accepted *dto.CreateDirectoryInput
			readCalls, downstreamCalls := 0, 0
			router := NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
			authorize := middleware.RequireAuthorization(func(_ context.Context, token string) (*domain.Actor, error) {
				if token != "fixture-access" {
					return nil, domain.ErrSessionNotFound
				}
				return &domain.Actor{UserID: 7, DeviceID: 13, SessionID: 17}, nil
			}, handler.AuthorizationFailure)
			router.POST("/test-create-directory-request", authorize, func(requestContext *gin.Context) {
				readCalls++
				var request dto.CreateDirectoryRequest
				if !binding.ReadJSON(requestContext, &request) {
					return
				}
				input, fieldError := dto.ParseCreateDirectoryRequest(request)
				if fieldError != nil {
					handler.InvalidArgument(requestContext, fieldError)
					return
				}
				accepted = input
				requestContext.AbortWithStatus(204)
			}, func(*gin.Context) { downstreamCalls++ })
			request := httptest.NewRequest("POST", "/test-create-directory-request?parent_id=private-parent&name=private-query&user_id=99", strings.NewReader(testCase.body))
			contentType := testCase.contentType
			if contentType == "" {
				contentType = "application/json"
			}
			request.Header.Set("Content-Type", contentType)
			request.Header.Set("Origin", "http://127.0.0.1:5173")
			request.Header.Set("X-Request-ID", "28EE758A-0727-42DE-AE7D-3BB33155E148")
			if testCase.encoding != "" {
				request.Header.Set("Content-Encoding", testCase.encoding)
			}
			if !testCase.noAuthorization {
				token := "fixture-access"
				if testCase.badToken {
					token = "private-bad-access"
				}
				request.Header.Set("Authorization", "Bearer "+token)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			// 2. 核对转换结果、统一字段错误、请求 ID、CORS 和日志保护。
			if response.Code != testCase.status || downstreamCalls != 0 || response.Header().Get("X-Request-ID") != "28ee758a-0727-42de-ae7d-3bb33155e148" || response.Header().Get("Access-Control-Allow-Origin") != "http://127.0.0.1:5173" {
				t.Fatal("HTTP 状态、处理结束、请求 ID 或 CORS 错误", response.Code)
			}
			if testCase.status == 401 && readCalls != 0 {
				t.Fatal("认证失败后仍读取了正文")
			}
			if testCase.status == 204 {
				if accepted == nil || accepted.ParentID != testCase.parentID || accepted.Name != testCase.entryName || (accepted.ColorKey != nil) != testCase.hasColor || (testCase.hasColor && *accepted.ColorKey != testCase.color) || response.Body.Len() != 0 {
					t.Fatal("解析结果或颜色空值错误")
				}
			} else {
				if accepted != nil {
					t.Fatal("错误请求进入了后续流程")
				}
				code := testCase.code
				if code == "" {
					code = "INVALID_ARGUMENT"
				}
				var body dto.ErrorResponse
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Error.Code != code || body.RequestID != response.Header().Get("X-Request-ID") || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("统一错误响应错误", err)
				}
				if testCase.field == "" {
					if body.Error.Details != nil {
						t.Fatal("绑定或认证错误应保留空 details")
					}
				} else if len(body.Error.Details) != 1 || body.Error.Details["field"] != testCase.field {
					t.Fatal("字段错误定位错误")
				}
			}
			if strings.Contains(logs.String(), "private-") || strings.Contains(response.Body.String(), "private-") || strings.Contains(logs.String(), "学习资料") {
				t.Fatal("日志或错误回显了正文、查询参数或凭证")
			}
		})
	}
}

func TestCreateDirectoryInputNameRules(t *testing.T) {
	for _, testCase := range []struct {
		rawName, displayName, key string
	}{
		{rawName: "  Cafe\u0301  ", displayName: "Cafe\u0301", key: "café"},
		{rawName: "Straße", displayName: "Straße", key: "strasse"},
		{rawName: strings.Repeat(" ", 256) + "a", displayName: "a", key: "a"},
		{rawName: "   "},
		{rawName: "资料/一"},
		{rawName: "CON.txt"},
		{rawName: strings.Repeat("🌱", 61)},
	} {
		// DTO 保留原始文本，后续名称服务执行完整业务规则。
		input, fieldError := dto.ParseCreateDirectoryRequest(dto.CreateDirectoryRequest{ParentID: "11", Name: testCase.rawName})
		if fieldError != nil || input == nil || input.Name != testCase.rawName {
			t.Fatal("名称服务调用前的请求解析错误", fieldError)
		}
		name, err := service.ValidateEntryName(input.Name)
		if testCase.displayName == "" {
			if err != domain.ErrInvalidEntryName || name != "" {
				t.Fatal("完整名称规则未拒绝非法名称", err)
			}
			continue
		}
		if err != nil || name != testCase.displayName {
			t.Fatal("名称服务未正确去空白或保留 Unicode 原文", err)
		}
		key, err := service.BuildEntryNameKey(name)
		if err != nil || string(key) != testCase.key {
			t.Fatal("DTO 与统一名称键规则衔接错误", err)
		}
	}
}
