package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestUploadReceiverLimitRealHTTP(t *testing.T) {
	directory := t.TempDir()
	receiver, err := service.NewUploadContentFileService(directory, service.NewUploadTempSpace())
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	var accepted atomic.Int32
	entered := make(chan struct{}, 3)
	proceed := make(chan struct{})
	router := NewRouter(logging.New(&logs), nil, func(context.Context) bool { return true })
	protected := middleware.RequireAuthorization(func(_ context.Context, token string) (*domain.Actor, error) {
		if token != "fixture-access" {
			return nil, domain.ErrInvalidAccessToken
		}
		return &domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}, nil
	}, handler.AuthorizationFailure)
	router.PUT("/test-receiver-limit", protected, func(requestContext *gin.Context) {
		// 1. 同一接收入口处理实际 HTTP 请求，暂停前三个文件使用回调。
		if !binding.ValidateUploadContentHeaders(requestContext) || !binding.ValidateUploadContentLength(requestContext, 0) {
			return
		}
		tracked := &uploadContentTrackedBody{reader: requestContext.Request.Body}
		err := receiver.WithFile(requestContext.Request.Context(), tracked, 0, nil, func(io.ReadSeeker, domain.UploadContentDigest) error {
			accepted.Add(1)
			entered <- struct{}{}
			<-proceed
			return nil
		})
		if err == nil {
			requestContext.Status(http.StatusNoContent)
			return
		}
		if !errors.Is(err, domain.ErrUploadReceiverBusy) {
			handler.InternalError(requestContext, nil)
			return
		}
		// 2. 名额满时正文读取为零，仅存在前三次接收的文件。
		entries, readError := os.ReadDir(directory)
		if readError != nil || len(entries) != 3 || tracked.reads != 0 || tracked.closes != 0 {
			t.Error("繁忙拒绝阶段产生正文或文件操作", readError)
		}
		requestContext.Header("Cache-Control", "no-store")
		requestContext.Header("Pragma", "no-cache")
		requestContext.AbortWithStatusJSON(http.StatusConflict, dto.ErrorResponse{
			Error:     dto.ErrorDetail{Code: "UPLOAD_BUSY", Message: domain.ErrUploadReceiverBusy.Error(), Details: nil},
			RequestID: requestcontext.RequestID(requestContext.Request.Context()),
		})
	})
	server := httptest.NewServer(router)
	defer server.Close()
	var callers sync.WaitGroup
	var finishOnce sync.Once
	finish := func() {
		finishOnce.Do(func() { close(proceed) })
		callers.Wait()
	}
	defer finish()
	client := server.Client()
	client.Timeout = 5 * time.Second
	type uploadResult struct {
		status  int
		body    []byte
		headers http.Header
		err     error
	}
	send := func() uploadResult {
		request, err := http.NewRequest(http.MethodPut, server.URL+"/test-receiver-limit?token=private-query", nil)
		if err != nil {
			return uploadResult{err: err}
		}
		request.Header.Set("Authorization", "Bearer fixture-access")
		request.Header.Set("Content-Type", "application/octet-stream")
		response, err := client.Do(request)
		if err != nil {
			return uploadResult{err: err}
		}
		payload, readError := io.ReadAll(response.Body)
		closeError := response.Body.Close()
		return uploadResult{response.StatusCode, payload, response.Header, errors.Join(readError, closeError)}
	}
	results := make(chan uploadResult, 3)
	for range 3 {
		callers.Go(func() { results <- send() })
	}
	for range 3 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("实际 HTTP 接收未进入前三个回调")
		}
	}
	// 3. 第四个零字节请求立即得到关联请求 ID 的固定繁忙响应。
	busy := send()
	if busy.err != nil || busy.status != http.StatusConflict || accepted.Load() != 3 {
		t.Fatal("第四个 HTTP 接收未按名额拒绝", busy.status, busy.err)
	}
	var result dto.ErrorResponse
	if err := json.Unmarshal(busy.body, &result); err != nil || result.Error.Code != "UPLOAD_BUSY" || result.Error.Message != "上传接收繁忙，请稍后重试" || result.Error.Details != nil || result.RequestID == "" || result.RequestID != busy.headers.Get("X-Request-ID") || busy.headers.Get("Cache-Control") != "no-store" || busy.headers.Get("Pragma") != "no-cache" {
		t.Fatal("接收繁忙公开契约错误", err)
	}
	// 4. 前三次接收清理完成后，后续请求可以重新进入。
	finish()
	for range 3 {
		completed := <-results
		if completed.err != nil || completed.status != http.StatusNoContent {
			t.Fatal("持有名额的请求完成失败", completed.status, completed.err)
		}
	}
	retry := send()
	entries, readError := os.ReadDir(directory)
	if retry.err != nil || retry.status != http.StatusNoContent || accepted.Load() != 4 || readError != nil || len(entries) != 0 {
		t.Fatal("名额复用或文件清理错误", retry.status, retry.err, readError)
	}
	for _, private := range []string{directory, "fixture-access", "private-query"} {
		if strings.Contains(logs.String(), private) || strings.Contains(string(busy.body), private) {
			t.Fatal("繁忙响应或日志泄漏私有输入")
		}
	}
}
