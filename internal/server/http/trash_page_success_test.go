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
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
)

const trashPageSuccessTestPath = "/test-trash-page-success"

func trashPageSuccessFixture() *domain.TrashEntryPageResult {
	directory := directoryPageSuccessFixture()
	file := directory.Entries[0]
	file.Status, file.Name = "TRASHED", "private-item 学习笔记.md"
	deletedAt := time.Date(2026, 11, 20, 4, 30, 0, 123456000, time.UTC)
	return &domain.TrashEntryPageResult{
		Entries: []domain.TrashEntryListItem{{
			Entry:        domain.TrashedFileEntry{FileEntry: file, DeletedAt: deletedAt},
			OriginalPath: directory.Path,
		}},
		NextCursor: directory.NextCursor,
	}
}

func TestTrashPageSuccessHTTP(t *testing.T) {
	const origin = "http://127.0.0.1:5173"
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	for _, testCase := range []struct {
		name               string
		suppliedID         string
		empty, last, isDir bool
	}{
		{name: "file page and supplied ID", suppliedID: strings.ToUpper(requestID)},
		{name: "final directory page", last: true, isDir: true},
		{name: "budget empty page", empty: true},
		{name: "final empty page", empty: true, last: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 使用真实 TCP 测试路由交付分页数据，预置待替换的旧正文头。
			result := trashPageSuccessFixture()
			if testCase.empty {
				result.Entries = nil
			}
			if testCase.last {
				result.NextCursor = nil
			}
			if testCase.isDir {
				result.Entries[0].Entry.EntryType, result.Entries[0].Entry.FileSize = "DIR", 0
			}
			expected, err := dto.NewTrashPageData(result)
			if err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			router := NewRouter(logging.New(&logs), []string{origin}, func(context.Context) bool { return true })
			router.GET(trashPageSuccessTestPath, func(requestContext *gin.Context) {
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
				requestContext.Header("Content-Type", "application/octet-stream")
				handler.TrashPageSuccess(requestContext, result)
			}, func(*gin.Context) { t.Error("成功响应发出后仍执行下游") })
			server := httptest.NewServer(router)
			defer server.Close()
			client := server.Client()
			client.Timeout = 3 * time.Second
			request, err := http.NewRequest(http.MethodGet, server.URL+trashPageSuccessTestPath+"?cursor=private-query", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Accept-Encoding", "identity")
			request.Header.Set("Origin", origin)
			request.Header.Set("Authorization", "Bearer private-access")
			if testCase.suppliedID != "" {
				request.Header.Set("X-Request-ID", testCase.suppliedID)
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, readError := io.ReadAll(response.Body)
			closeError := response.Body.Close()
			server.Close()

			// 2. 核对收到的 JSON、缓存控制头和请求 ID。
			header := response.Header
			if readError != nil || closeError != nil || response.StatusCode != http.StatusOK ||
				response.ContentLength != int64(len(body)) || header.Get("Content-Encoding") != "" ||
				header.Get("Content-Type") != "application/json; charset=utf-8" ||
				header.Get("Cache-Control") != "no-store" || header.Get("Pragma") != "no-cache" ||
				header.Get("Access-Control-Allow-Origin") != origin || len(header.Values("Set-Cookie")) != 0 {
				t.Fatalf("分页状态为 %d，响应头为 %v，读取错误为 %v", response.StatusCode, header, readError)
			}
			actualID := header.Get("X-Request-ID")
			parsedID, err := uuid.Parse(actualID)
			if err != nil || parsedID.Version() != 4 || (testCase.suppliedID != "" && actualID != requestID) {
				t.Fatalf("响应请求 ID 为 %q，错误为 %v", actualID, err)
			}
			want, err := json.Marshal(dto.TrashPageResponse{Data: *expected, RequestID: actualID})
			if err != nil || !bytes.Equal(body, want) {
				t.Fatalf("收到正文 %s，期望 %s，错误为 %v", body, want, err)
			}
			var recorded map[string]any
			if err := json.Unmarshal(logs.Bytes(), &recorded); err != nil || recorded["request_id"] != actualID ||
				recorded["status"] != float64(http.StatusOK) || strings.Contains(logs.String(), "private-") ||
				strings.Contains(logs.String(), "fixture-signed-next-cursor") {
				t.Fatal("完成日志错误或记录了回收项名称及游标", err)
			}
		})
	}
}

func TestTrashPageSuccessFailureHTTP(t *testing.T) {
	for _, scenario := range []string{"nil result", "invalid path", "canceled on entry", "canceled before write"} {
		t.Run(scenario, func(t *testing.T) {
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.GET(trashPageSuccessTestPath, func(requestContext *gin.Context) {
				// 1. 模拟输出错误或请求取消，检查错误响应能清理旧正文头。
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
				result := trashPageSuccessFixture()
				ctx, cancel := context.WithCancel(requestContext.Request.Context())
				defer cancel()
				switch scenario {
				case "nil result":
					result = nil
				case "invalid path":
					result.Entries[0].OriginalPath.Entries[1].Status = "PURGING"
				case "canceled on entry":
					cancel()
					requestContext.Request = requestContext.Request.WithContext(ctx)
				case "canceled before write":
					requestContext.Request = requestContext.Request.WithContext(&cancelBeforeRefreshWriteContext{Context: ctx, cancel: cancel})
				}
				handler.TrashPageSuccess(requestContext, result)
			}, func(*gin.Context) { t.Error("失败响应发出后仍执行下游") })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, trashPageSuccessTestPath, nil))
			var failure dto.ErrorResponse
			header := response.Header()
			if response.Code != http.StatusInternalServerError || json.Unmarshal(response.Body.Bytes(), &failure) != nil ||
				failure.Error.Code != "INTERNAL_ERROR" || failure.Error.Details != nil ||
				failure.RequestID == "" || failure.RequestID != header.Get("X-Request-ID") ||
				header.Get("Cache-Control") != "no-store" || header.Get("Pragma") != "no-cache" ||
				header.Get("Content-Type") != "application/json; charset=utf-8" ||
				header.Get("Content-Encoding") != "" || header.Get("Content-Length") != "" ||
				strings.Contains(response.Body.String(), "private-") || strings.Contains(response.Body.String(), `"data"`) {
				t.Fatalf("统一错误状态为 %d，响应头为 %v，正文为 %s", response.Code, header, response.Body.String())
			}
		})
	}
}

func TestTrashPageSuccessEndedResponse(t *testing.T) {
	for _, alreadyWritten := range []bool{true, false} {
		name := "aborted response"
		if alreadyWritten {
			name = "written response"
		}
		t.Run(name, func(t *testing.T) {
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			var wantBody string
			router.GET(trashPageSuccessTestPath, func(requestContext *gin.Context) {
				// 1. 先结束响应，再调用成功处理函数以验证不会重复写出。
				requestContext.Header("Cache-Control", "private")
				if alreadyWritten {
					wantBody = "existing response"
					requestContext.String(http.StatusConflict, wantBody)
				} else {
					requestContext.Status(http.StatusConflict)
					requestContext.Abort()
				}
				handler.TrashPageSuccess(requestContext, trashPageSuccessFixture())
			}, func(*gin.Context) { t.Error("已结束的响应继续执行了下游") })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, trashPageSuccessTestPath, nil))
			if response.Code != http.StatusConflict || response.Body.String() != wantBody ||
				response.Header().Get("Cache-Control") != "private" || response.Header().Get("Pragma") != "" {
				t.Fatalf("已结束响应变为 %d、%v、%s", response.Code, response.Header(), response.Body.String())
			}
		})
	}
}
