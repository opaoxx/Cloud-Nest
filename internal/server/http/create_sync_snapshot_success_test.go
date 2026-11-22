package http

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
)

const createSyncSnapshotFixturePath = "/test-create-sync-snapshot-success"

func TestCreateSyncSnapshotSuccessHTTP(t *testing.T) {
	for _, scenario := range []string{"with next page", "single page", "empty tree"} {
		t.Run(scenario, func(t *testing.T) {
			// 1. 准备创建服务已确认交付的首页，通过独立回环 POST 写出。
			result := syncSnapshotPageSuccessFixture()
			if scenario != "with next page" {
				result.NextCursor, result.BaselineToken = nil, new("fixture-baseline.fixture-signature")
			}
			if scenario == "empty tree" {
				result.Items, result.Header.HighWaterSequence = nil, 0
			}
			expected, err := dto.NewSyncSnapshotPageData(result)
			if err != nil {
				t.Fatal(err)
			}
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.POST(createSyncSnapshotFixturePath, func(requestContext *gin.Context) {
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
				handler.CreateSyncSnapshotSuccess(requestContext, result)
			}, func(*gin.Context) { t.Error("首页写出后进入下游") })
			server := httptest.NewServer(router)
			defer server.Close()
			client := server.Client()
			client.Timeout = 2 * time.Second

			// 2. 对同一个首页写出两次，核对 201、页面原值与每次的请求 ID。
			var originalData json.RawMessage
			for _, requestID := range []string{"28ee758a-0727-42de-ae7d-3bb33155e148", "28ee758a-0727-42de-ae7d-3bb33155e149"} {
				request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+createSyncSnapshotFixturePath, nil)
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("X-Request-ID", requestID)
				response, err := client.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				payload, readError := io.ReadAll(response.Body)
				response.Body.Close()
				if readError != nil || response.StatusCode != http.StatusCreated ||
					response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Pragma") != "no-cache" ||
					response.Header.Get("Content-Type") != "application/json; charset=utf-8" ||
					response.Header.Get("Content-Encoding") != "" || response.ContentLength != int64(len(payload)) {
					t.Fatalf("创建首页状态=%d，读取错误=%v，响应头=%v", response.StatusCode, readError, response.Header)
				}
				var body dto.SyncSnapshotPageResponse
				if err := json.Unmarshal(payload, &body); err != nil || !reflect.DeepEqual(body.Data, *expected) ||
					body.RequestID != requestID || response.Header.Get("X-Request-ID") != requestID {
					t.Fatalf("创建首页的页面或请求 ID 错误：%s，%v", payload, err)
				}
				var envelope map[string]json.RawMessage
				if json.Unmarshal(payload, &envelope) != nil || len(envelope) != 2 || envelope["data"] == nil {
					t.Fatalf("创建首页响应字段不符：%s", payload)
				}
				if originalData == nil {
					originalData = envelope["data"]
				} else if !bytes.Equal(originalData, envelope["data"]) {
					t.Fatal("同一首页的 data 随请求 ID 变化")
				}
			}
		})
	}
}

func TestCreateSyncSnapshotSuccessFailureAndCommittedResponse(t *testing.T) {
	// 1. 非法结果和写出前取消仍遵循公共错误契约，已有响应保持原值。
	for _, scenario := range []string{"invalid page", "canceled before write", "already committed"} {
		t.Run(scenario, func(t *testing.T) {
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.POST(createSyncSnapshotFixturePath, func(requestContext *gin.Context) {
				result := syncSnapshotPageSuccessFixture()
				switch scenario {
				case "invalid page":
					result.BaselineToken = new("fixture-baseline.fixture-signature")
				case "canceled before write":
					ctx, cancel := context.WithCancel(requestContext.Request.Context())
					defer cancel()
					requestContext.Request = requestContext.Request.WithContext(&cancelBeforeRefreshWriteContext{Context: ctx, cancel: cancel})
				case "already committed":
					requestContext.String(http.StatusAccepted, "already written")
				}
				handler.CreateSyncSnapshotSuccess(requestContext, result)
			}, func(*gin.Context) { t.Error("结束响应后进入下游") })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, createSyncSnapshotFixturePath, nil))
			if scenario == "already committed" {
				if response.Code != http.StatusAccepted || response.Body.String() != "already written" {
					t.Fatal("已提交的响应被改写")
				}
				return
			}
			var body dto.ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != http.StatusInternalServerError ||
				body.Error.Code != "INTERNAL_ERROR" || body.Error.Details != nil || body.RequestID == "" ||
				body.RequestID != response.Header().Get("X-Request-ID") || response.Header().Get("Cache-Control") != "no-store" ||
				strings.Contains(response.Body.String(), "fixture-signature") {
				t.Fatalf("创建首页错误响应为 %d，正文 %s，解析错误 %v", response.Code, response.Body.String(), err)
			}
		})
	}
}
