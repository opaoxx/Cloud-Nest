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
	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

const syncSnapshotPageFixturePath = "/test-sync-snapshot-page-success"

func syncSnapshotPageSuccessFixture() *domain.SyncSnapshotPageResult {
	return &domain.SyncSnapshotPageResult{
		SnapshotID: uuid.MustParse("052dc4f7-f4c1-46b3-a509-bf96992f6580"),
		Header:     domain.SyncSnapshotHeader{RootEntryID: 11, HighWaterSequence: 42},
		ExpiresAt:  time.Date(2026, 11, 23, 2, 10, 0, 123456789, time.UTC),
		Items:      []json.RawMessage{json.RawMessage(`{"entry_id":"12","parent_id":"11","name":"学习资料","entry_type":"DIR","revision":"3","size_bytes":0,"sha256":null}`)},
		NextCursor: new("fixture-payload.fixture-signature"),
	}
}

func TestSyncSnapshotPageSuccessDelivery(t *testing.T) {
	// 1. 通过带公共中间件的测试入口交付续页、末页和空树。
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	for _, scenario := range []string{"continuation", "last page", "empty tree"} {
		t.Run(scenario, func(t *testing.T) {
			result := syncSnapshotPageSuccessFixture()
			if scenario != "continuation" {
				result.NextCursor, result.BaselineToken = nil, new("fixture-baseline.fixture-signature")
			}
			if scenario == "empty tree" {
				result.Items = nil
				result.Header.HighWaterSequence = 0
			}
			expected, err := dto.NewSyncSnapshotPageData(result)
			if err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			var contextID string
			router := NewRouter(logging.New(&logs), nil, func(context.Context) bool { return true })
			router.GET(syncSnapshotPageFixturePath, func(requestContext *gin.Context) {
				contextID = requestcontext.RequestID(requestContext.Request.Context())
				requestContext.Header("Content-Type", "application/octet-stream")
				requestContext.Header("Content-Encoding", "gzip")
				requestContext.Header("Content-Length", "3")
				handler.SyncSnapshotPageSuccess(requestContext, result)
			}, func(*gin.Context) { t.Error("快照交付后进入下游") })
			request := httptest.NewRequest(http.MethodGet, syncSnapshotPageFixturePath+"?cursor=private-test-query", nil)
			if scenario == "continuation" {
				request.Header.Set("X-Request-ID", strings.ToUpper(requestID))
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			// 2. 核对状态码、缓存与正文头，以及上下文、响应头和 JSON 的同一个请求 ID。
			if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" ||
				response.Header().Get("Content-Type") != "application/json; charset=utf-8" || response.Header().Get("Content-Encoding") != "" ||
				response.Header().Get("Content-Length") == "3" {
				t.Fatal("成功状态或响应头不符")
			}
			var body dto.SyncSnapshotPageResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || !reflect.DeepEqual(body.Data, *expected) ||
				body.RequestID != contextID || body.RequestID != response.Header().Get("X-Request-ID") {
				t.Fatalf("页面数据或请求 ID 不符，错误=%v", err)
			}
			id, err := uuid.Parse(body.RequestID)
			if err != nil || id.Version() != 4 || id.Variant() != uuid.RFC4122 || (scenario == "continuation" && body.RequestID != requestID) {
				t.Fatalf("请求 ID 未正确生成或规范化，错误=%v", err)
			}
			var outer map[string]json.RawMessage
			var data map[string]json.RawMessage
			if json.Unmarshal(response.Body.Bytes(), &outer) != nil || len(outer) != 2 || json.Unmarshal(outer["data"], &data) != nil || len(data) != 7 ||
				(result.NextCursor == nil && string(data["next_cursor"]) != "null") ||
				(result.BaselineToken == nil && string(data["baseline_token"]) != "null") ||
				(scenario == "empty tree" && string(data["items"]) != "[]") {
				t.Fatal("公开响应缺少规定字段或空值编码错误")
			}
			if strings.Contains(logs.String(), "private-test-query") || strings.Contains(logs.String(), "fixture-signature") {
				t.Fatal("完成日志不应包含凭据或原始查询")
			}
		})
	}
}

func TestSyncSnapshotPageSuccessFailureAndCommittedResponse(t *testing.T) {
	// 1. 验证无效页面、写出前取消和已提交响应的处理。
	for _, scenario := range []string{"invalid page", "canceled before write", "already committed"} {
		t.Run(scenario, func(t *testing.T) {
			result := syncSnapshotPageSuccessFixture()
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.GET(syncSnapshotPageFixturePath, func(requestContext *gin.Context) {
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
				handler.SyncSnapshotPageSuccess(requestContext, result)
			}, func(*gin.Context) { t.Error("终止或失败后进入下游") })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, syncSnapshotPageFixturePath, nil))
			if scenario == "already committed" {
				if response.Code != http.StatusAccepted || response.Body.String() != "already written" || response.Header().Get("Cache-Control") != "" {
					t.Fatal("已提交的响应被改写")
				}
				return
			}
			var body dto.ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != http.StatusInternalServerError ||
				body.Error.Code != "INTERNAL_ERROR" || body.Error.Details != nil || body.RequestID == "" ||
				body.RequestID != response.Header().Get("X-Request-ID") || response.Header().Get("Cache-Control") != "no-store" ||
				strings.Contains(response.Body.String(), "fixture-signature") {
				t.Fatalf("失败响应不符合统一错误契约，错误=%v", err)
			}
		})
	}
}

func TestSyncSnapshotPageSuccessHTTP(t *testing.T) {
	// 1. 用系统分配的回环端口验证真实 GET 传输和响应体完整性。
	router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
	router.GET(syncSnapshotPageFixturePath, func(requestContext *gin.Context) {
		requestContext.Header("Content-Length", "3")
		handler.SyncSnapshotPageSuccess(requestContext, syncSnapshotPageSuccessFixture())
	})
	server := httptest.NewServer(router)
	defer server.Close()
	client := server.Client()
	client.Timeout = 2 * time.Second
	response, err := client.Get(server.URL + syncSnapshotPageFixturePath)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" || response.ContentLength != int64(len(payload)) {
		t.Fatalf("回环 HTTP 交付不完整，错误=%v", err)
	}
	var body dto.SyncSnapshotPageResponse
	if err := json.Unmarshal(payload, &body); err != nil || body.RequestID == "" || body.RequestID != response.Header.Get("X-Request-ID") ||
		body.Data.NextCursor == nil || body.Data.BaselineToken != nil {
		t.Fatalf("回环响应的数据或请求 ID 不符，错误=%v", err)
	}
}
