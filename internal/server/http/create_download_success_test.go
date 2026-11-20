package http

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
)

const downloadSuccessTestPath = "/test-create-download-success"

func createDownloadSuccessFixture() *domain.EntryDownloadAuthorization {
	return &domain.EntryDownloadAuthorization{
		Link: domain.SignedDownloadURL{
			URL:       "https://storage.example.invalid/bucket/note%2B.txt?X-Amz-Signature=private-link&versionId=old%2BA",
			ExpiresAt: time.Date(2026, 11, 20, 16, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60)),
		},
		ObjectID: math.MaxUint64, Revision: 9007199254740993, SizeBytes: 4, VerifiedSHA256: sha256.Sum256([]byte("note")),
		MIMEType:    "text/plain",
		Disposition: `attachment; filename="download.txt"; filename*=UTF-8''%E5%AD%A6%E4%B9%A0.txt`,
	}
}

func TestCreateDownloadSuccessHTTP(t *testing.T) {
	const origin = "http://127.0.0.1:5173"
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	for _, scenario := range []struct {
		name                       string
		providedRequestID, preview bool
	}{
		{name: "download with supplied ID", providedRequestID: true},
		{name: "preview with generated ID", preview: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 使用真实 TCP 入口，模拟进入下载交付前已有旧正文头。
			result := createDownloadSuccessFixture()
			if scenario.preview {
				result.Disposition = strings.Replace(result.Disposition, "attachment", "inline", 1)
			}
			wantData, err := dto.NewCreateDownloadData(result)
			if err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			router := NewRouter(logging.New(&logs), []string{origin}, func(context.Context) bool { return true })
			router.POST(downloadSuccessTestPath, func(requestContext *gin.Context) {
				requestContext.Header("Content-Type", "application/octet-stream")
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
				handler.CreateDownloadSuccess(requestContext, result)
			}, func(*gin.Context) { t.Error("交付之后继续执行了下游") })
			server := httptest.NewServer(router)
			defer server.Close()
			client := server.Client()
			client.Timeout = 3 * time.Second
			request, err := http.NewRequest(http.MethodPost, server.URL+downloadSuccessTestPath+"?note=private-query", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Origin", origin)
			request.Header.Set("Authorization", "Bearer private-credential")
			if scenario.providedRequestID {
				request.Header.Set("X-Request-ID", strings.ToUpper(requestID))
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal("真实下载交付请求失败", err)
			}
			body, readError := io.ReadAll(response.Body)
			closeError := response.Body.Close()
			server.Close()

			// 2. 核对禁止缓存头、请求 ID、八字段正文及完成日志。
			if readError != nil || closeError != nil || response.StatusCode != http.StatusOK || int64(len(body)) != response.ContentLength ||
				response.Header.Get("Content-Type") != "application/json; charset=utf-8" || response.Header.Get("Content-Encoding") != "" ||
				response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Pragma") != "no-cache" ||
				response.Header.Get("Access-Control-Allow-Origin") != origin || response.Header.Get("Location") != "" {
				t.Fatal("下载 HTTP 状态、正文或响应头错误", response.StatusCode, readError, closeError)
			}
			actualID := response.Header.Get("X-Request-ID")
			parsedID, err := uuid.Parse(actualID)
			if err != nil || parsedID.Version() != 4 || (scenario.providedRequestID && actualID != requestID) {
				t.Fatal("下载响应请求 ID 错误", actualID, err)
			}
			wantBody, err := json.Marshal(dto.CreateDownloadResponse{Data: *wantData, RequestID: actualID})
			if err != nil || !bytes.Equal(body, wantBody) {
				t.Fatal("下载 JSON 未保留完整的公开授权和请求 ID")
			}
			var logged map[string]any
			if json.Unmarshal(logs.Bytes(), &logged) != nil || logged["request_id"] != actualID ||
				logged["status"] != float64(http.StatusOK) || logged["path"] != downloadSuccessTestPath ||
				strings.Contains(logs.String(), "private-") || strings.Contains(string(body), "private-credential") {
				t.Fatal("下载完成日志与请求 ID 不符或包含敏感信息")
			}
		})
	}
}

func TestCreateDownloadSuccessFailures(t *testing.T) {
	for _, scenario := range []string{"nil authorization", "invalid URL", "zero revision", "invalid MIME", "canceled", "deadline", "cancel before write"} {
		t.Run(scenario, func(t *testing.T) {
			// 1. 将异常授权或取消状态直接交给 HTTP 交付方法。
			result := createDownloadSuccessFixture()
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.POST(downloadSuccessTestPath, func(requestContext *gin.Context) {
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
				ctx := requestContext.Request.Context()
				switch scenario {
				case "nil authorization":
					result = nil
				case "invalid URL":
					result.Link.URL = "/private-link"
				case "zero revision":
					result.Revision = 0
				case "invalid MIME":
					result.MIMEType = "IMAGE/PNG"
				case "canceled":
					canceled, stop := context.WithCancel(ctx)
					stop()
					requestContext.Request = requestContext.Request.WithContext(canceled)
				case "deadline":
					expired, stop := context.WithDeadline(ctx, time.Unix(0, 0))
					defer stop()
					requestContext.Request = requestContext.Request.WithContext(expired)
				case "cancel before write":
					cancelable, stop := context.WithCancel(ctx)
					defer stop()
					requestContext.Request = requestContext.Request.WithContext(&cancelBeforeRefreshWriteContext{Context: cancelable, cancel: stop})
				}
				handler.CreateDownloadSuccess(requestContext, result)
			}, func(*gin.Context) { t.Error("失败响应发出后执行了下游") })

			// 2. 核对统一错误、当前请求 ID 和禁止缓存，保密签名链接。
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, downloadSuccessTestPath, nil))
			var body dto.ErrorResponse
			header := response.Header()
			if response.Code != http.StatusInternalServerError || json.Unmarshal(response.Body.Bytes(), &body) != nil ||
				body.Error.Code != "INTERNAL_ERROR" || body.Error.Details != nil || body.RequestID == "" || body.RequestID != header.Get("X-Request-ID") ||
				strings.Contains(response.Body.String(), "private-link") || strings.Contains(response.Body.String(), `"data"`) ||
				header.Get("Content-Length") != "" || header.Get("Content-Encoding") != "" ||
				header.Get("Content-Type") != "application/json; charset=utf-8" || header.Get("Cache-Control") != "no-store" || header.Get("Pragma") != "no-cache" {
				t.Fatal("下载异常交付不符", response.Code)
			}
		})
	}
}

func TestCreateDownloadSuccessEndedResponse(t *testing.T) {
	for _, written := range []bool{true, false} {
		router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
		wantBody := ""
		if written {
			wantBody = "existing response"
		}
		router.POST(downloadSuccessTestPath, func(requestContext *gin.Context) {
			// 1. 先结束响应，再尝试交付授权。
			requestContext.Header("Cache-Control", "private")
			if written {
				requestContext.String(http.StatusConflict, wantBody)
			} else {
				requestContext.Status(http.StatusConflict)
				requestContext.Abort()
			}
			handler.CreateDownloadSuccess(requestContext, createDownloadSuccessFixture())
		}, func(*gin.Context) { t.Error("响应结束后继续执行了下游") })
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, downloadSuccessTestPath, nil))
		if response.Code != http.StatusConflict || response.Body.String() != wantBody ||
			response.Header().Get("Cache-Control") != "private" || response.Header().Get("Pragma") != "" {
			t.Fatal("下载交付改写了已经结束的响应", response.Code)
		}
	}
}
