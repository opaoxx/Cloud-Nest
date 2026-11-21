package storage

import (
	"context"
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

func multipartAbortTestClient(t *testing.T, abort http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Query().Has("location"):
			_, _ = io.WriteString(writer, "<LocationConstraint>us-east-1</LocationConstraint>")
		case request.Method == http.MethodHead && strings.Trim(request.URL.Path, "/") == "cloudnest":
			writer.WriteHeader(http.StatusOK)
		case request.Method == http.MethodDelete:
			abort(writer, request)
		default:
			t.Error("出现计划之外的存储操作", request.Method)
			writer.WriteHeader(http.StatusBadRequest)
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

func TestAbortMultipartUploadS3Response(t *testing.T) {
	const key, session = "users/7/objects/abort +%.bin", "Opaque+A/= "
	const missingBody = "<Error><Code>NoSuchUpload</Code><Message>private fixture</Message></Error>"
	for _, fixture := range []struct {
		name, body, requestID string
		status                int
		want                  error
		dropResponse, cancel  bool
	}{
		{name: "acknowledged abort", status: http.StatusNoContent},
		{name: "explicit session missing", status: http.StatusNotFound, body: missingBody, requestID: "fixture-request", want: domain.ErrUploadPartSessionMissing},
		{name: "missing without storage request ID", status: http.StatusNotFound, body: missingBody, want: domain.ErrUploadStorageUnavailable},
		{name: "generic 404", status: http.StatusNotFound, body: "private fixture", requestID: "fixture-request", want: domain.ErrUploadStorageUnavailable},
		{name: "missing bucket", status: http.StatusNotFound, body: "<Error><Code>NoSuchBucket</Code></Error>", requestID: "fixture-request", want: domain.ErrUploadStorageUnavailable},
		{name: "missing code with wrong status", status: http.StatusForbidden, body: missingBody, requestID: "fixture-request", want: domain.ErrUploadStorageUnavailable},
		{name: "unexpected 200", status: http.StatusOK, want: domain.ErrUploadStorageUnavailable},
		{name: "temporary error", status: http.StatusServiceUnavailable, body: "private fixture", want: domain.ErrUploadStorageUnavailable},
		{name: "lost response", dropResponse: true, want: domain.ErrUploadStorageUnavailable},
		{name: "canceled in flight", cancel: true, status: http.StatusNoContent, want: domain.ErrUploadStorageUnavailable},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var calls atomic.Int32
			client := multipartAbortTestClient(t, func(writer http.ResponseWriter, request *http.Request) {
				// 1. 核对实际中止请求使用固定桶及未改写的对象键和会话 ID。
				calls.Add(1)
				query := request.URL.Query()
				if request.URL.Path != "/cloudnest/"+key || len(query) != 1 || query.Get("uploadId") != session ||
					!strings.HasPrefix(request.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
					t.Error("中止请求未绑定原对象和会话")
				}
				if fixture.dropResponse {
					connection, _, err := writer.(http.Hijacker).Hijack()
					if err != nil {
						t.Error("无法模拟响应丢失", err)
						return
					}
					_ = connection.Close()
					return
				}
				if fixture.cancel {
					cancel()
				}
				writer.Header().Set("Content-Type", "application/xml")
				if fixture.requestID != "" {
					writer.Header().Set("X-Amz-Request-Id", fixture.requestID)
				}
				writer.WriteHeader(fixture.status)
				if fixture.body != "" {
					_, _ = io.WriteString(writer, fixture.body)
				}
			})

			// 2. 核对结果分类、取消信号及请求次数，确认错误不回显存储消息。
			err := client.AbortMultipartUpload(ctx, key, session)
			if !errors.Is(err, fixture.want) || calls.Load() != 1 ||
				errors.Is(err, domain.ErrUploadPartSessionMissing) != (fixture.want == domain.ErrUploadPartSessionMissing) ||
				errors.Is(err, context.Canceled) != fixture.cancel ||
				(err != nil && strings.Contains(err.Error(), "private fixture")) {
				t.Fatal("中止结果或实际请求次数不符", err, calls.Load())
			}
		})
	}
}

func TestAbortMultipartUploadRejectsInvalidInput(t *testing.T) {
	var calls atomic.Int32
	client := multipartAbortTestClient(t, func(writer http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writer.WriteHeader(http.StatusNoContent)
	})
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	expired, stop := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer stop()
	for _, fixture := range []struct {
		name, key, session string
		ctx                context.Context
		want               error
	}{
		{name: "empty key", session: "session", ctx: t.Context(), want: domain.ErrInvalidStoredObjectData},
		{name: "control in key", key: "key\n", session: "session", ctx: t.Context(), want: domain.ErrInvalidStoredObjectData},
		{name: "unbound session", key: "key", ctx: t.Context(), want: domain.ErrInvalidUploadTaskData},
		{name: "control in session", key: "key", session: "session\n", ctx: t.Context(), want: domain.ErrInvalidUploadTaskData},
		{name: "already canceled", key: "key", session: "session", ctx: canceled, want: context.Canceled},
		{name: "deadline already passed", key: "key", session: "session", ctx: expired, want: context.DeadlineExceeded},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			if err := client.AbortMultipartUpload(fixture.ctx, fixture.key, fixture.session); !errors.Is(err, fixture.want) ||
				errors.Is(err, domain.ErrUploadStorageUnavailable) {
				t.Fatal("无效输入或开始前取消的结果不符", err)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("无效输入触发了存储中止请求", calls.Load())
	}
}
