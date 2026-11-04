package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

func TestRequestIDInputs(t *testing.T) {
	// 1. 覆盖客户端合法标识和需要替换的格式，不允许解析器的宽松格式绕过规则。
	const validID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	testCases := []struct {
		name   string
		values []string
		wantID string
	}{
		{name: "missing"},
		{name: "empty", values: []string{""}},
		{name: "valid lower", values: []string{validID}, wantID: validID},
		{name: "valid upper", values: []string{strings.ToUpper(validID)}, wantID: validID},
		{name: "surrounding whitespace", values: []string{" " + validID + " "}},
		{name: "wrong UUID version", values: []string{"28ee758a-0727-12de-ae7d-3bb33155e148"}},
		{name: "wrong UUID variant", values: []string{"28ee758a-0727-42de-0e7d-3bb33155e148"}},
		{name: "nil UUID", values: []string{uuid.Nil.String()}},
		{name: "URN", values: []string{"urn:uuid:" + validID}},
		{name: "braces", values: []string{"{" + validID + "}"}},
		{name: "raw hex", values: []string{strings.ReplaceAll(validID, "-", "")}},
		{name: "invalid hex", values: []string{"gggggggg-0727-42de-ae7d-3bb33155e148"}},
		{name: "too long", values: []string{strings.Repeat("a", 4096)}},
		{name: "comma combined", values: []string{validID + "," + validID}},
		{name: "duplicate values", values: []string{validID, validID}},
		{name: "mixed values", values: []string{validID, "invalid"}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			router := gin.New()
			router.Use(RequestID())
			router.GET("/test", func(requestContext *gin.Context) {
				// 2. 处理函数和响应必须获得同一个最终 ID，而不是客户端原始值。
				requestID := requestcontext.RequestID(requestContext.Request.Context())
				if requestID == "" || requestID != requestContext.Request.Header.Get(RequestIDHeader) || requestID != requestContext.Writer.Header().Get(RequestIDHeader) || len(requestContext.Request.Header.Values(RequestIDHeader)) != 1 {
					t.Error("上下文、请求头和响应头中的 ID 不一致")
				}
				requestContext.Status(http.StatusNoContent)
			})
			request := httptest.NewRequest(http.MethodGet, "/test", nil)
			for _, value := range testCase.values {
				request.Header.Add(RequestIDHeader, value)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			requestID := response.Header().Get(RequestIDHeader)
			parsedID, err := uuid.Parse(requestID)
			if response.Code != http.StatusNoContent || len(requestID) != 36 || err != nil || parsedID.Version() != 4 || parsedID.Variant() != uuid.RFC4122 {
				t.Fatal("最终响应未保持原状态或缺少标准 UUID v4")
			}
			if testCase.wantID != "" && requestID != testCase.wantID {
				t.Fatal("合法 ID 未按约定保留并规范化")
			}
			if testCase.wantID == "" && len(testCase.values) > 0 && requestID == testCase.values[0] {
				t.Fatal("无效或重复的输入未被替换")
			}
		})
	}
}

func TestRequestIDConcurrentIsolation(t *testing.T) {
	// 1. 多个并发请求使用同一个路由器，处理函数回传自身上下文中的 ID。
	router := gin.New()
	router.Use(RequestID())
	router.GET("/test", func(requestContext *gin.Context) {
		requestContext.Header("X-Test-Context-ID", requestcontext.RequestID(requestContext.Request.Context()))
		requestContext.Status(http.StatusNoContent)
	})
	const requestCount = 32
	responses := make(chan *httptest.ResponseRecorder, requestCount)
	for range requestCount {
		go func() {
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/test", nil))
			responses <- response
		}()
	}

	// 2. 每个请求单独生成 ID，上下文与响应一致，不在请求间共享状态。
	seen := make(map[string]bool)
	for range requestCount {
		response := <-responses
		requestID := response.Header().Get(RequestIDHeader)
		if response.Code != http.StatusNoContent || requestID == "" || requestID != response.Header().Get("X-Test-Context-ID") || seen[requestID] {
			t.Fatal("并发请求的 ID 缺失、重复或与上下文不一致")
		}
		seen[requestID] = true
	}
}
