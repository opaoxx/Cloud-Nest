package http

import (
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

const baselineSuccessFixturePath = "/test-confirm-sync-snapshot-baseline-success"

func TestConfirmSyncSnapshotBaselineSuccessHTTP(t *testing.T) {
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	for _, testCase := range []struct {
		name, sequence, incomingID string
		position                   uint64
	}{
		{name: "zero with supplied request ID", sequence: "0", incomingID: strings.ToUpper(requestID)},
		{name: "uint64 limit with generated request ID", sequence: "18446744073709551615", position: math.MaxUint64},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 通过回环 POST 夹具走公共中间件，并预置需要清理的正文头。
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.POST(baselineSuccessFixturePath, func(requestContext *gin.Context) {
				requestContext.Header("Content-Type", "application/octet-stream")
				requestContext.Header("Content-Encoding", "gzip")
				requestContext.Header("Content-Length", "3")
				handler.ConfirmSyncSnapshotBaselineSuccess(requestContext, testCase.position)
			}, func(*gin.Context) { t.Error("写出后进入了下游处理函数") })
			server := httptest.NewServer(router)
			defer server.Close()
			client := server.Client()
			client.Timeout = 2 * time.Second
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+baselineSuccessFixturePath, nil)
			if err != nil {
				t.Fatal(err)
			}
			if testCase.incomingID != "" {
				request.Header.Set("X-Request-ID", testCase.incomingID)
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()

			// 2. 核对完整正文、状态和响应头，确认外层 ID 与响应头一致。
			payload, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" ||
				response.Header.Get("Pragma") != "no-cache" || response.Header.Get("Content-Type") != "application/json; charset=utf-8" ||
				response.Header.Get("Content-Encoding") != "" || response.Uncompressed || response.ContentLength != int64(len(payload)) {
				t.Fatalf("回环响应状态、响应头或正文长度不符：%d, %v, %v", response.StatusCode, response.Header, err)
			}
			responseID := response.Header.Get("X-Request-ID")
			id, err := uuid.Parse(responseID)
			if err != nil || id.Version() != 4 || id.Variant() != uuid.RFC4122 || id.String() != responseID ||
				(testCase.incomingID != "" && responseID != requestID) {
				t.Fatalf("请求 ID 未正确生成或规范化：%q，错误 %v", responseID, err)
			}
			var body dto.ConfirmSyncSnapshotBaselineResponse
			if err := json.Unmarshal(payload, &body); err != nil || body.RequestID != responseID || body.Data.Status != "ACTIVE" || body.Data.AckSequence != testCase.sequence {
				t.Fatalf("公开确认数据不符：%s，错误 %v", payload, err)
			}
		})
	}
}

func TestConfirmSyncSnapshotBaselineSuccessWriteBoundaries(t *testing.T) {
	for _, scenario := range []string{"canceled before encoding", "canceled before write", "already committed"} {
		t.Run(scenario, func(t *testing.T) {
			// 1. 分别模拟编码前取消、编码后取消和已有响应。
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.POST(baselineSuccessFixturePath, func(requestContext *gin.Context) {
				if scenario == "already committed" {
					requestContext.String(http.StatusAccepted, "already written")
					handler.ConfirmSyncSnapshotBaselineSuccess(requestContext, 17)
					return
				}
				ctx, cancel := context.WithCancel(requestContext.Request.Context())
				defer cancel()
				if scenario == "canceled before encoding" {
					cancel()
					requestContext.Request = requestContext.Request.WithContext(ctx)
				} else {
					requestContext.Request = requestContext.Request.WithContext(&cancelBeforeRefreshWriteContext{Context: ctx, cancel: cancel})
				}
				handler.ConfirmSyncSnapshotBaselineSuccess(requestContext, 17)
			}, func(*gin.Context) { t.Error("写出停止后进入了下游处理函数") })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, baselineSuccessFixturePath, nil))

			// 2. 已发送内容保持原样，取消时只交付统一内部错误。
			if scenario == "already committed" {
				if response.Code != http.StatusAccepted || response.Body.String() != "already written" || response.Header().Get("Cache-Control") != "" {
					t.Fatal("已发送响应被改写")
				}
				return
			}
			var body dto.ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != http.StatusInternalServerError ||
				body.Error.Code != "INTERNAL_ERROR" || body.Error.Details != nil || body.RequestID == "" ||
				body.RequestID != response.Header().Get("X-Request-ID") || response.Header().Get("Cache-Control") != "no-store" ||
				strings.Contains(response.Body.String(), `"ack_sequence"`) {
				t.Fatalf("取消响应不符合统一错误契约：%d, %s, %v", response.Code, response.Body.String(), err)
			}
		})
	}
}
