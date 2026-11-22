package handler

import (
	"crypto/sha256"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

const reuseEmptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
const reuseRequestJSON = `{"parent_id":"18446744073709551615","name":"  学习笔记.PDF  ","size_bytes":0,"sha256":"` + reuseEmptySHA256 + `"}`

func TestReadReuseFileRequest(t *testing.T) {
	for _, scenario := range []struct {
		label, body string
		sizeBytes   uint64
	}{
		{"zero bytes and lowercase digest", reuseRequestJSON, 0},
		{"safe maximum and uppercase digest", strings.Replace(
			strings.Replace(reuseRequestJSON, reuseEmptySHA256, strings.ToUpper(reuseEmptySHA256), 1),
			`"size_bytes":0`, `"size_bytes":9007199254740991`, 1,
		), 1<<53 - 1},
	} {
		t.Run(scenario.label, func(t *testing.T) {
			// 1. 通过内存 HTTP 请求提供正文与大小写敏感的幂等键。
			request := httptest.NewRequest(http.MethodPost, "/fixture", strings.NewReader(scenario.body))
			request.Header.Set("Content-Type", "application/json; charset=UTF-8")
			request.Header.Set("Idempotency-Key", "  Reuse-A\t")
			router := gin.New()
			router.POST("/fixture", func(requestContext *gin.Context) {
				// 2. 核对类型转换与名称原文，合法参数才进入下游。
				input, key := readReuseFileRequest(requestContext)
				if input == nil || key != "Reuse-A" || input.ParentID != math.MaxUint64 ||
					input.Name != "  学习笔记.PDF  " || input.SizeBytes != scenario.sizeBytes || input.SHA256 != sha256.Sum256(nil) {
					t.Fatalf("绑定参数不符，得到 %+v 和键 %q", input, key)
				}
				requestContext.Status(http.StatusNoContent)
			})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != http.StatusNoContent {
				t.Fatalf("正常请求未通过，得到 %d 和 %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestReadReuseFileRequestRejectsInvalidInput(t *testing.T) {
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	const parent = `"parent_id":"18446744073709551615"`
	const name = `"name":"  学习笔记.PDF  "`
	const size = `"size_bytes":0`
	const digest = `"sha256":"` + reuseEmptySHA256 + `"`
	for _, scenario := range []struct {
		label, old, replacement, field string
		keys                           []string
		status                         int
	}{
		{label: "missing key before malformed JSON", old: reuseRequestJSON, replacement: "!", field: "Idempotency-Key", keys: []string{}},
		{label: "repeated key", field: "Idempotency-Key", keys: []string{"one", "two"}},
		{label: "ambiguous key", field: "Idempotency-Key", keys: []string{"one,two"}},
		{label: "missing parent", old: parent + ",", field: "parent_id"},
		{label: "numeric parent", old: parent, replacement: `"parent_id":11`, field: "parent_id"},
		{label: "noncanonical parent", old: parent, replacement: `"parent_id":"011"`, field: "parent_id"},
		{label: "overflowing parent", old: parent, replacement: `"parent_id":"18446744073709551616"`, field: "parent_id"},
		{label: "missing name", old: name + ",", field: "name"},
		{label: "nonstring name", old: name, replacement: `"name":false`, field: "name"},
		{label: "empty name", old: name, replacement: `"name":""`, field: "name"},
		{label: "missing size", old: size + ",", field: "size_bytes"},
		{label: "null size", old: size, replacement: `"size_bytes":null`, field: "size_bytes"},
		{label: "negative size", old: size, replacement: `"size_bytes":-1`, field: "size_bytes"},
		{label: "decimal size", old: size, replacement: `"size_bytes":1.0`, field: "size_bytes"},
		{label: "exponential size", old: size, replacement: `"size_bytes":1e3`, field: "size_bytes"},
		{label: "string size", old: size, replacement: `"size_bytes":"0"`, field: "size_bytes"},
		{label: "unsafe size", old: size, replacement: `"size_bytes":9007199254740992`, field: "size_bytes"},
		{label: "missing digest", old: "," + digest, field: "sha256"},
		{label: "null digest", old: digest, replacement: `"sha256":null`, field: "sha256"},
		{label: "invalid digest text", old: digest, replacement: `"sha256":"` + strings.Repeat("z", 64) + `"`, field: "sha256"},
		{label: "digest whitespace", old: digest, replacement: `"sha256":"` + reuseEmptySHA256 + ` "`, field: "sha256"},
		{label: "unknown field", old: digest, replacement: digest + `,"unexpected":true`},
		{label: "trailing JSON", old: reuseRequestJSON, replacement: reuseRequestJSON + `{}`},
		{label: "invalid UTF-8", old: name, replacement: "\"name\":\"bad\xff\""},
		{label: "body over limit", old: reuseRequestJSON, replacement: reuseRequestJSON + strings.Repeat(" ", int(binding.MaxJSONBodyBytes)), status: http.StatusRequestEntityTooLarge},
	} {
		t.Run(scenario.label, func(t *testing.T) {
			// 1. 只改变当前用例要验证的输入或请求头。
			body := reuseRequestJSON
			if scenario.old != "" {
				body = strings.Replace(body, scenario.old, scenario.replacement, 1)
			}
			request := httptest.NewRequest(http.MethodPost, "/fixture", strings.NewReader(body)).
				WithContext(requestcontext.WithRequestID(t.Context(), requestID))
			request.Header.Set("Content-Type", "application/json")
			keys := scenario.keys
			if keys == nil {
				keys = []string{"Reuse-A"}
			}
			for _, key := range keys {
				request.Header.Add("Idempotency-Key", key)
			}

			// 2. 错误应阻止参数交付与下游处理，并关联本次请求。
			router := gin.New()
			router.POST("/fixture", func(requestContext *gin.Context) {
				if input, key := readReuseFileRequest(requestContext); input != nil || key != "" {
					t.Fatal("非法请求仍交付了参数", input, key)
				}
			}, func(*gin.Context) { t.Error("非法请求仍进入下游") })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			wantStatus, wantCode := http.StatusBadRequest, "INVALID_ARGUMENT"
			if scenario.status != 0 {
				wantStatus, wantCode = scenario.status, "REQUEST_BODY_TOO_LARGE"
			}
			var result dto.ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || response.Code != wantStatus ||
				result.Error.Code != wantCode || result.Error.Message == "" || result.RequestID != requestID ||
				response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("错误响应不符，得到 %d、%s 和 %v", response.Code, response.Body.String(), err)
			}
			if scenario.field != "" && result.Error.Details["field"] != scenario.field {
				t.Fatalf("错误字段不符，得到 %+v，期望 %q", result.Error.Details, scenario.field)
			}
		})
	}
}
