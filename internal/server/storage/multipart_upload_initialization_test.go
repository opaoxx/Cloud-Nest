package storage

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const multipartInitializationKey = "users/7/objects/fixture"

func multipartInitializationClient(t *testing.T, initialize http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Query().Has("location"):
			response.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(response, "<LocationConstraint>us-east-1</LocationConstraint>")
		case request.Method == http.MethodHead && strings.Trim(request.URL.Path, "/") == "cloudnest":
			response.WriteHeader(http.StatusOK)
		case request.Method == http.MethodPost && request.URL.Query().Has("uploads"):
			initialize(response, request)
		default:
			t.Error("出现计划以外的 MinIO 操作", request.Method)
			response.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(t.Context(), storageTestConfig(t, server.URL))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client
}

func TestCreateMultipartUploadS3Response(t *testing.T) {
	for _, fixture := range []struct {
		name, uploadID string
		status         int
		want           error
	}{
		{"opaque ID", "session+id/=", 200, nil},
		{"max column length", strings.Repeat("x", 1024), 200, nil},
		{"missing ID", "", 200, domain.ErrUploadStorageUnavailable},
		{"blank ID", "   ", 200, domain.ErrUploadStorageUnavailable},
		{"non ASCII", "会话", 200, domain.ErrUploadStorageUnavailable},
		{"control", "line\nbreak", 200, domain.ErrUploadStorageUnavailable},
		{"too long", strings.Repeat("x", 1025), 200, domain.ErrUploadStorageUnavailable},
		{"denied", "", 403, domain.ErrUploadStorageUnavailable},
		{"service unavailable", "", 503, domain.ErrUploadStorageUnavailable},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			var calls atomic.Int32
			client := multipartInitializationClient(t, func(response http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				// 1. 核对固定对象键、签名及分片校验声明。
				if request.URL.Path != "/cloudnest/"+multipartInitializationKey ||
					request.Header.Get("X-Amz-Checksum-Algorithm") != "SHA256" ||
					request.Header.Get("Content-Type") != "application/octet-stream" ||
					!strings.HasPrefix(request.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
					t.Error("分片初始化请求未绑定预期对象或校验算法")
				}
				response.Header().Set("Content-Type", "application/xml")
				response.WriteHeader(fixture.status)
				if fixture.status != 200 {
					_, _ = io.WriteString(response, "<Error><Code>AccessDenied</Code><Message>private-storage-response</Message></Error>")
					return
				}
				_, _ = io.WriteString(response, "<InitiateMultipartUploadResult><UploadId>")
				_ = xml.EscapeText(response, []byte(fixture.uploadID))
				_, _ = io.WriteString(response, "</UploadId></InitiateMultipartUploadResult>")
			})
			id, err := client.CreateMultipartUpload(t.Context(), multipartInitializationKey)
			if !errors.Is(err, fixture.want) || calls.Load() != 1 {
				t.Fatalf("初始化结果或请求次数不符: %v，调用 %d 次", err, calls.Load())
			}
			if (err == nil && id != fixture.uploadID) || (err != nil && (id != "" || strings.Contains(err.Error(), "private-"))) {
				t.Fatal("会话原值或故障脱敏不符")
			}
		})
	}
}

func TestCreateMultipartUploadInputAndCancellation(t *testing.T) {
	var calls atomic.Int32
	requestStarted := make(chan struct{}, 1)
	client := multipartInitializationClient(t, func(response http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		requestStarted <- struct{}{}
		select {
		case <-request.Context().Done():
		case <-time.After(2 * time.Second):
			t.Error("模拟存储请求未按期取消")
			response.WriteHeader(http.StatusBadRequest)
		}
	})
	for _, key := range []string{"", " ", "bad\x00key", "文件", strings.Repeat("x", 256)} {
		if id, err := client.CreateMultipartUpload(t.Context(), key); id != "" || !errors.Is(err, domain.ErrInvalidStoredObjectData) {
			t.Fatal("无效对象键被接受", err)
		}
	}
	cancelled, stop := context.WithCancel(t.Context())
	stop()
	if id, err := client.CreateMultipartUpload(cancelled, multipartInitializationKey); id != "" || !errors.Is(err, context.Canceled) || calls.Load() != 0 {
		t.Fatal("预取消调用不应发起创建", err, calls.Load())
	}
	expired, releaseExpired := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer releaseExpired()
	if id, err := client.CreateMultipartUpload(expired, multipartInitializationKey); id != "" || !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 0 {
		t.Fatal("已超过期限的调用不应发起创建", err, calls.Load())
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cancelledRequest := make(chan struct{})
	go func() {
		defer close(cancelledRequest)
		select {
		case <-requestStarted:
			cancel()
		case <-ctx.Done():
		}
	}()
	id, err := client.CreateMultipartUpload(ctx, multipartInitializationKey)
	cancel()
	<-cancelledRequest
	if id != "" || !errors.Is(err, context.Canceled) || calls.Load() != 1 {
		t.Fatal("在途取消未返回统一取消错误", err, calls.Load())
	}
}
