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

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
)

func TestRouteNotFoundResponseAndCorrelation(t *testing.T) {
	// 1. 未匹配路径及当前未注册的方法都经过全局处理链，不访问依赖。
	const validID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	testCases := []struct {
		name      string
		method    string
		target    string
		requestID string
		wantID    string
	}{
		{name: "generated ID", method: "GET", target: "/missing"},
		{name: "supplied ID", method: "GET", target: "/api/v1/missing", requestID: validID, wantID: validID},
		{name: "normalized ID", method: "POST", target: "/missing?token=private-query", requestID: strings.ToUpper(validID), wantID: validID},
		{name: "replaced ID", method: "DELETE", target: "/missing", requestID: "invalid"},
		{name: "unregistered method", method: "POST", target: "/ping", requestID: validID, wantID: validID},
		{name: "unregistered options", method: "OPTIONS", target: "/missing"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			var logs bytes.Buffer
			router := NewRouter(logging.New(&logs), func(ctx context.Context) bool {
				t.Error("未匹配请求不应调用依赖检查")
				return true
			})
			request := httptest.NewRequest(testCase.method, testCase.target, strings.NewReader("private-request-body"))
			if testCase.requestID != "" {
				request.Header.Set("X-Request-ID", testCase.requestID)
			}
			request.Header.Set("Cookie", "session=private-cookie")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			// 2. 错误字段固定，details 必须明确为 null，且无默认文本尾随。
			if response.Code != http.StatusNotFound || !strings.HasPrefix(response.Header().Get("Content-Type"), "application/json") || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("404 状态或响应头不符合契约")
			}
			decoder := json.NewDecoder(response.Body)
			var body map[string]json.RawMessage
			if err := decoder.Decode(&body); err != nil {
				t.Fatal(err)
			}
			var extra any
			if err := decoder.Decode(&extra); err != io.EOF || len(body) != 2 || body["error"] == nil || body["request_id"] == nil {
				t.Fatal("错误响应字段不完整或附带了额外文本")
			}
			var detail map[string]json.RawMessage
			if err := json.Unmarshal(body["error"], &detail); err != nil {
				t.Fatal(err)
			}
			var code, message, requestID string
			if err := json.Unmarshal(detail["code"], &code); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(detail["message"], &message); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(body["request_id"], &requestID); err != nil {
				t.Fatal(err)
			}
			if len(detail) != 3 || code != "ROUTE_NOT_FOUND" || message != "未找到匹配的接口" || string(detail["details"]) != "null" {
				t.Fatal("错误码、消息或 null 语义不符合契约")
			}
			if strings.Contains(string(body["error"]), "private-") {
				t.Fatal("错误正文回显了请求数据")
			}

			// 3. 正文、响应头和唯一完成日志使用同一个最终 UUID v4。
			parsedID, err := uuid.Parse(requestID)
			if err != nil || len(requestID) != 36 || parsedID.Version() != 4 || requestID != response.Header().Get("X-Request-ID") {
				t.Fatal("错误正文没有使用最终请求 ID")
			}
			if testCase.wantID != "" && requestID != testCase.wantID {
				t.Fatal("合法输入 ID 未保留或规范化")
			}
			logDecoder := json.NewDecoder(&logs)
			var record map[string]any
			if err := logDecoder.Decode(&record); err != nil {
				t.Fatal(err)
			}
			if err := logDecoder.Decode(&extra); err != io.EOF || record["request_id"] != requestID || record["status"] != float64(404) || record["level"] != "WARN" || record["method"] != testCase.method || record["path"] != request.URL.Path {
				t.Fatal("错误响应与完成日志未正确关联")
			}
		})
	}
}
