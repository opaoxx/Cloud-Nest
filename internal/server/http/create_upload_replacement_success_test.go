package http

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
)

func replacementSuccessSnapshot(t *testing.T, mode string) json.RawMessage {
	t.Helper()
	stamp := time.Date(2020, 1, 2, 3, 4, 5, 123456000, time.UTC)
	digest := sha256.Sum256([]byte("synthetic replacement"))
	task := domain.UploadTask{
		ID: 42, UserID: 7, DeviceID: 11, StagedObjectID: 23, TargetParentID: 13, RequestedName: "资料.bin",
		UploadMode: "SINGLE", Status: domain.UploadStatusUploading,
		FileSize: domain.MaxSingleUploadSizeBytes, ReservedBytes: domain.MaxSingleUploadSizeBytes, ExpectedSHA256: &digest,
		ReplacementEntryID: new(uint64(31)), ReplacementExpectedRevision: new(uint64(6)),
		CreatedAt: stamp, UpdatedAt: stamp, ExpiresAt: stamp.Add(24 * time.Hour),
	}
	if mode == "MULTIPART" {
		task.UploadMode, task.Status = "MULTIPART", domain.UploadStatusCreating
		task.FileSize, task.ReservedBytes = 50*1024*1024*1024, 50*1024*1024*1024
		task.PartSize, task.PartCount = new(uint64(32*1024*1024)), new(uint32(1600))
	}
	body, err := dto.EncodeCreateUploadReplacementSnapshot(&task)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestCreateUploadReplacementSuccessHTTP(t *testing.T) {
	const route = "/test-create-upload-replacement-success"
	for _, mode := range []string{"SINGLE", "MULTIPART"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 启动回环测试路由，重复交付同一份历史创建快照。
			stableBody := replacementSuccessSnapshot(t, mode)
			var formatted bytes.Buffer
			if err := json.Indent(&formatted, stableBody, "", "  "); err != nil {
				t.Fatal(err)
			}
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.POST(route, func(requestContext *gin.Context) {
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
				requestContext.Header("Content-Type", "application/octet-stream")
				handler.CreateUploadReplacementSuccess(requestContext, formatted.Bytes())
			}, func(*gin.Context) { t.Error("创建成功交付后进入下游") })
			server := httptest.NewServer(router)
			defer server.Close()
			client := server.Client()
			client.Timeout = 3 * time.Second

			for _, requestID := range []string{"28EE758A-0727-42DE-AE7D-3BB33155E148", "28EE758A-0727-42DE-AE7D-3BB33155E149"} {
				request, err := http.NewRequest(http.MethodPost, server.URL+route, nil)
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("X-Request-ID", requestID)
				request.Header.Set("Accept-Encoding", "identity")
				response, err := client.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				encoded, readError := io.ReadAll(response.Body)
				closeError := response.Body.Close()
				if readError != nil || closeError != nil || response.StatusCode != http.StatusCreated || response.ContentLength != int64(len(encoded)) ||
					response.Header.Get("Content-Type") != "application/json; charset=utf-8" || response.Header.Get("Content-Encoding") != "" ||
					response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Pragma") != "no-cache" {
					t.Fatalf("创建 HTTP 交付不符，状态 %d，读取 %v，关闭 %v", response.StatusCode, readError, closeError)
				}

				// 2. 保留模式、绑定、路径或分片计划，仅用本次请求的标识组装响应。
				var outer map[string]json.RawMessage
				var currentID string
				if json.Unmarshal(encoded, &outer) != nil || len(outer) != 2 || json.Unmarshal(outer["request_id"], &currentID) != nil ||
					currentID != strings.ToLower(requestID) || currentID != response.Header.Get("X-Request-ID") ||
					!bytes.Equal(outer["data"], stableBody) {
					t.Fatalf("稳定快照或本次请求 ID 交付错误 %s", encoded)
				}
			}
		})
	}
}

func TestCreateUploadReplacementSuccessFailures(t *testing.T) {
	const route = "/test-create-upload-replacement-success"
	singleBody := replacementSuccessSnapshot(t, "SINGLE")
	multipartBody := replacementSuccessSnapshot(t, "MULTIPART")
	ordinaryBody, err := json.Marshal(createUploadSuccessFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		name string
		body []byte
	}{
		{"malformed snapshot", []byte("private-value")},
		{"unknown mode", bytes.Replace(singleBody, []byte(`"SINGLE"`), []byte(`"UNKNOWN"`), 2)},
		{"ordinary snapshot", ordinaryBody},
		{"wrong single path", bytes.Replace(singleBody, []byte("/uploads/42/content"), []byte("/uploads/43/content"), 1)},
		{"wrong multipart plan", bytes.Replace(multipartBody, []byte(`"part_count":1600`), []byte(`"part_count":1599`), 1)},
		{"missing initial result", bytes.Replace(singleBody, []byte(`,"published_revision":null`), nil, 1)},
		{"cancelled", singleBody},
		{"cancel before write", multipartBody},
		{"already written", singleBody},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 在测试路由制造快照错误、取消或已经写出的响应。
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.POST(route, func(requestContext *gin.Context) {
				if fixture.name == "already written" {
					requestContext.String(http.StatusAccepted, "fixture response")
				} else {
					requestContext.Header("Content-Length", "3")
					requestContext.Header("Content-Encoding", "gzip")
				}
				ctx, cancel := context.WithCancel(requestContext.Request.Context())
				defer cancel()
				if fixture.name == "cancelled" {
					cancel()
					requestContext.Request = requestContext.Request.WithContext(ctx)
				}
				if fixture.name == "cancel before write" {
					requestContext.Request = requestContext.Request.WithContext(&cancelBeforeRefreshWriteContext{Context: ctx, cancel: cancel})
				}
				handler.CreateUploadReplacementSuccess(requestContext, fixture.body)
			}, func(*gin.Context) { t.Error("交付结束后进入下游") })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, route, nil))

			// 2. 保护已发送内容，其他失败只交付统一错误及当前请求 ID。
			if fixture.name == "already written" {
				if response.Code != http.StatusAccepted || response.Body.String() != "fixture response" || response.Header().Get("Cache-Control") != "" {
					t.Fatalf("已发送的响应被改写，状态 %d，正文 %s", response.Code, response.Body.String())
				}
				return
			}
			var reply dto.ErrorResponse
			if response.Code != http.StatusInternalServerError || json.Unmarshal(response.Body.Bytes(), &reply) != nil ||
				reply.Error.Code != "INTERNAL_ERROR" || reply.Error.Details != nil || reply.RequestID == "" || reply.RequestID != response.Header().Get("X-Request-ID") ||
				response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" ||
				response.Header().Get("Content-Encoding") != "" || response.Header().Get("Content-Length") != "" ||
				strings.Contains(response.Body.String(), "private-value") || strings.Contains(response.Body.String(), "replacement") {
				t.Fatalf("失败响应不符，状态 %d，正文 %s", response.Code, response.Body.String())
			}
		})
	}
}
