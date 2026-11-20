package http

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

const purgePreviewSuccessTestPath = "/test-purge-preview-success"

func purgePreviewSuccessFixture(t *testing.T) *domain.PurgePreview {
	t.Helper()
	signer, err := service.NewPurgePreviewSigner(bytes.Repeat([]byte{9}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := service.BuildPurgePreviewSubtreeFingerprint([]domain.PurgePreviewSubtreeEntry{{
		ID: math.MaxUint64, Revision: 4, EntryType: "FILE", Status: "TRASHED", FileSize: math.MaxUint64,
	}})
	if err != nil {
		t.Fatal(err)
	}
	issuedAt := time.Date(2026, 11, 20, 4, 30, 0, 123456000, time.UTC)
	credential, err := signer.Sign(7, math.MaxUint64, fingerprint, issuedAt)
	if err != nil {
		t.Fatal(err)
	}
	return &domain.PurgePreview{
		EntryID: math.MaxUint64, Revision: 4,
		Summary:    domain.PurgePreviewSummary{FileCount: 1, LogicalBytes: math.MaxUint64},
		Credential: credential,
	}
}

func TestPurgePreviewSuccessHTTP(t *testing.T) {
	// 1. 通过测试路由发出真正的 HTTP 请求，模拟前置处理留下的正文头。
	result := purgePreviewSuccessFixture(t)
	var logs bytes.Buffer
	router := NewRouter(logging.New(&logs), nil, func(context.Context) bool { return true })
	router.GET(purgePreviewSuccessTestPath, func(requestContext *gin.Context) {
		requestContext.Header("Content-Length", "3")
		requestContext.Header("Content-Encoding", "gzip")
		requestContext.Header("Content-Type", "application/octet-stream")
		handler.PurgePreviewSuccess(requestContext, result)
	}, func(*gin.Context) { t.Error("交付预检后继续执行了下游") })
	server := httptest.NewServer(router)
	defer server.Close()
	client := server.Client()
	client.Timeout = 3 * time.Second

	const suppliedID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	for _, rawID := range []string{strings.ToUpper(suppliedID), ""} {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+purgePreviewSuccessTestPath+"?note=private-query", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Accept-Encoding", "identity")
		if rawID != "" {
			request.Header.Set("X-Request-ID", rawID)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, readError := io.ReadAll(response.Body)
		closeError := response.Body.Close()

		// 2. 核对实际收到的响应头、请求 ID 和完整 JSON。
		header := response.Header
		if readError != nil || closeError != nil || response.StatusCode != http.StatusOK ||
			response.ContentLength != int64(len(body)) || header.Get("Content-Encoding") != "" ||
			header.Get("Content-Type") != "application/json; charset=utf-8" ||
			header.Get("Cache-Control") != "no-store" || header.Get("Pragma") != "no-cache" {
			t.Fatalf("响应为 %d，响应头 %v，读取错误 %v，关闭错误 %v", response.StatusCode, header, readError, closeError)
		}
		actualID := header.Get("X-Request-ID")
		parsedID, err := uuid.Parse(actualID)
		if err != nil || parsedID.Version() != 4 || parsedID.Variant() != uuid.RFC4122 ||
			(rawID != "" && actualID != suppliedID) || (rawID == "" && actualID == suppliedID) {
			t.Fatal("请求 ID 未规范化或生成", actualID, err)
		}
		wantBody := fmt.Sprintf(`{"data":{"entry_id":"18446744073709551615","revision":"4","file_count":1,"directory_count":0,"logical_bytes":"18446744073709551615","preview_digest":%q,"expires_at":"2026-11-20T04:35:00.123456Z"},"request_id":%q}`,
			result.Credential.PreviewDigest, actualID)
		if string(body) != wantBody {
			t.Fatal("实际响应应交付完整精度、原样凭证及当次请求 ID")
		}
	}
	server.Close()

	// 3. 核对请求完成日志无签名凭证及查询内容。
	if strings.Contains(logs.String(), result.Credential.PreviewDigest) || strings.Contains(logs.String(), "private-query") {
		t.Fatal("日志包含凭证或查询内容")
	}
}

func TestPurgePreviewSuccessFailures(t *testing.T) {
	for _, scenario := range []string{"nil result", "invalid range", "canceled on entry", "canceled before write"} {
		t.Run(scenario, func(t *testing.T) {
			result := purgePreviewSuccessFixture(t)
			digest := result.Credential.PreviewDigest
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.GET(purgePreviewSuccessTestPath, func(requestContext *gin.Context) {
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
				ctx, cancel := context.WithCancel(requestContext.Request.Context())
				defer cancel()
				switch scenario {
				case "nil result":
					result = nil
				case "invalid range":
					result.Summary.DirectoryCount = domain.MaxFileEntrySubtreeNodes
				case "canceled on entry":
					cancel()
					requestContext.Request = requestContext.Request.WithContext(ctx)
				case "canceled before write":
					requestContext.Request = requestContext.Request.WithContext(&cancelBeforeRefreshWriteContext{Context: ctx, cancel: cancel})
				}
				handler.PurgePreviewSuccess(requestContext, result)
			}, func(*gin.Context) { t.Error("错误响应发出后仍执行了下游") })

			// 1. 核对统一错误正文和响应头，确保凭证不会出现在错误中。
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, purgePreviewSuccessTestPath, nil))
			var failure dto.ErrorResponse
			header := response.Header()
			if response.Code != http.StatusInternalServerError || json.Unmarshal(response.Body.Bytes(), &failure) != nil ||
				failure.Error.Code != "INTERNAL_ERROR" || failure.Error.Details != nil ||
				failure.RequestID == "" || failure.RequestID != header.Get("X-Request-ID") ||
				header.Get("Cache-Control") != "no-store" || header.Get("Pragma") != "no-cache" ||
				header.Get("Content-Type") != "application/json; charset=utf-8" || header.Get("Content-Encoding") != "" ||
				header.Get("Content-Length") != "" || strings.Contains(response.Body.String(), digest) ||
				strings.Contains(response.Body.String(), `"data"`) {
				t.Fatalf("预检错误响应不符，状态 %d，响应头 %v", response.Code, header)
			}
		})
	}
}

func TestPurgePreviewSuccessEndedResponse(t *testing.T) {
	result := purgePreviewSuccessFixture(t)
	for _, alreadyWritten := range []bool{true, false} {
		router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
		wantBody := ""
		router.GET(purgePreviewSuccessTestPath, func(requestContext *gin.Context) {
			requestContext.Header("Cache-Control", "private")
			if alreadyWritten {
				wantBody = "existing response"
				requestContext.String(http.StatusConflict, wantBody)
			} else {
				requestContext.Status(http.StatusConflict)
				requestContext.Abort()
			}
			handler.PurgePreviewSuccess(requestContext, result)
		}, func(*gin.Context) { t.Error("已结束的响应继续执行了下游") })
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, purgePreviewSuccessTestPath, nil))
		if response.Code != http.StatusConflict || response.Body.String() != wantBody ||
			response.Header().Get("Cache-Control") != "private" || response.Header().Get("Pragma") != "" {
			t.Fatal("已结束的响应被改写", response.Code, response.Header())
		}
	}
}
