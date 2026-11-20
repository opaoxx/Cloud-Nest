package http

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
)

const purgeAcceptedTestPath = "/test-purge-entry-accepted"
const purgeAcceptedTestProgressURL = "/api/v1/trash/18446744073709551615/purge"
const purgeAcceptedTestBody = `{ "status":"PURGING", "progress_url":"` + purgeAcceptedTestProgressURL + `", "entry_id":"18446744073709551615" }`

func TestPurgeEntryAcceptedHTTP(t *testing.T) {
	const suppliedID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	stableBody := []byte(purgeAcceptedTestBody)
	router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
	router.DELETE(purgeAcceptedTestPath, func(requestContext *gin.Context) {
		requestContext.Header("Content-Type", "application/octet-stream")
		requestContext.Header("Content-Length", "3")
		requestContext.Header("Content-Encoding", "gzip")
		handler.PurgeEntryAccepted(requestContext, math.MaxUint64, stableBody)
	}, func(*gin.Context) { t.Error("写出 202 后继续执行了下游") })
	server := httptest.NewServer(router)
	defer server.Close()
	client := server.Client()
	client.Timeout = 3 * time.Second

	// 1. 用同一快照应答两次，分别使用显式和自动生成的请求 ID。
	for _, rawRequestID := range []string{strings.ToUpper(suppliedID), ""} {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodDelete, server.URL+purgeAcceptedTestPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Accept-Encoding", "identity")
		if rawRequestID != "" {
			request.Header.Set("X-Request-ID", rawRequestID)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("读取或关闭响应失败：%v / %v", readErr, closeErr)
		}
		requestID := response.Header.Get("X-Request-ID")
		parsedID, err := uuid.Parse(requestID)
		if err != nil || parsedID.Version() != 4 || (requestID == suppliedID) != (rawRequestID != "") {
			t.Fatalf("响应请求 ID 不符：%q（请求中为 %q）", requestID, rawRequestID)
		}

		// 2. 核对 202、实际传输长度和完整公开正文。
		wantBody := `{"data":{"entry_id":"18446744073709551615","status":"PURGING","progress_url":"` +
			purgeAcceptedTestProgressURL + `"},"request_id":"` + requestID + `"}`
		if response.StatusCode != http.StatusAccepted || response.ContentLength != int64(len(body)) ||
			response.Header.Get("Content-Type") != "application/json; charset=utf-8" ||
			response.Header.Get("Content-Encoding") != "" || response.Header.Get("Set-Cookie") != "" ||
			response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Pragma") != "no-cache" ||
			string(body) != wantBody {
			t.Fatalf("受理响应不符：状态 %d，响应头 %v，正文 %s", response.StatusCode, response.Header, body)
		}
	}
	if !bytes.Equal(stableBody, []byte(purgeAcceptedTestBody)) {
		t.Fatal("输入快照被修改")
	}
}

func TestPurgeEntryAcceptedFailures(t *testing.T) {
	for _, scenario := range []string{"wrong progress path", "duplicate field", "canceled", "cancel before write"} {
		t.Run(scenario, func(t *testing.T) {
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.DELETE(purgeAcceptedTestPath, func(requestContext *gin.Context) {
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
				stableBody := purgeAcceptedTestBody
				switch scenario {
				case "wrong progress path":
					stableBody = strings.Replace(stableBody, purgeAcceptedTestProgressURL, "/api/v1/trash/23/purge", 1)
				case "duplicate field":
					stableBody = strings.Replace(stableBody, `"status":`, `"status":"PURGING","status":`, 1)
				case "canceled", "cancel before write":
					cancelable, cancel := context.WithCancel(requestContext.Request.Context())
					defer cancel()
					var ctx context.Context = cancelable
					if scenario == "canceled" {
						cancel()
					} else {
						ctx = &cancelBeforeRefreshWriteContext{Context: cancelable, cancel: cancel}
					}
					requestContext.Request = requestContext.Request.WithContext(ctx)
				}
				handler.PurgeEntryAccepted(requestContext, math.MaxUint64, []byte(stableBody))
			}, func(*gin.Context) { t.Error("写出错误后继续执行了下游") })

			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, purgeAcceptedTestPath, nil))
			var failure dto.ErrorResponse
			if response.Code != http.StatusInternalServerError || json.Unmarshal(response.Body.Bytes(), &failure) != nil ||
				failure.Error.Code != "INTERNAL_ERROR" || failure.Error.Details != nil ||
				failure.RequestID == "" || failure.RequestID != response.Header().Get("X-Request-ID") ||
				response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" ||
				response.Header().Get("Content-Type") != "application/json; charset=utf-8" ||
				response.Header().Get("Content-Length") != "" || response.Header().Get("Content-Encoding") != "" ||
				strings.Contains(response.Body.String(), `"data"`) || strings.Contains(response.Body.String(), "18446744073709551615") {
				t.Fatalf("异常响应不符：状态 %d，响应头 %v，正文 %s", response.Code, response.Header(), response.Body.String())
			}
		})
	}
}
