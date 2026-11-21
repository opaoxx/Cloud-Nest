package storage

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func multipartCompletionTestClient(t *testing.T, complete http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Query().Has("location"):
			_, _ = io.WriteString(writer, "<LocationConstraint>us-east-1</LocationConstraint>")
		case request.Method == http.MethodHead && strings.Trim(request.URL.Path, "/") == "cloudnest":
			writer.WriteHeader(http.StatusOK)
		case request.Method == http.MethodPost:
			complete(writer, request)
		default:
			t.Error("出现计划之外的存储操作", request.Method, request.URL.Path)
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

func multipartCompletionTestReceipts() []domain.UploadPartReceipt {
	const digest = "TgdAhWK+24tgzgXB3s/jrRa3IjCWfeAfZAt+Rym0n84="
	return []domain.UploadPartReceipt{
		{PartNumber: 1, SizeBytes: domain.MaxSingleUploadSizeBytes, ETag: "First", ChecksumSHA256: digest},
		{PartNumber: 2, SizeBytes: 1, ETag: "Tail", ChecksumSHA256: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="},
	}
}

func TestCompleteMultipartUploadS3Response(t *testing.T) {
	const key, session, etag = "users/7/objects/merge +%.bin", "Opaque+A/= ", "Complete-2"
	const start = "<CompleteMultipartUploadResult><Bucket>cloudnest</Bucket><Key>" + key + "</Key>"
	const success = start + `<ETag>"` + etag + `"</ETag></CompleteMultipartUploadResult>`
	const invalidPart = "<Error><Code>InvalidPart</Code><Message>private fixture</Message></Error>"
	const missingSession = "<Error><Code>NoSuchUpload</Code><Message>private fixture</Message></Error>"
	unknown := domain.ErrMultipartUploadCompletionUnknown
	for _, fixture := range []struct {
		name, body, version string
		status              int
		want                error
		missing, cancelIO   bool
		dropResponse        bool
	}{
		{name: "versioned success", body: success, version: "Version+A/="},
		{name: "unversioned success", body: success},
		{name: "explicit null version", body: success, version: "null"},
		{name: "wrong bucket", body: strings.Replace(success, "cloudnest", "foreign", 1), want: unknown},
		{name: "wrong key", body: strings.Replace(success, key, "foreign", 1), want: unknown},
		{name: "missing ETag", body: start + "</CompleteMultipartUploadResult>", want: unknown},
		{name: "invalid version", body: success, version: strings.Repeat("v", 256), want: unknown},
		{name: "truncated success", body: start, want: unknown},
		{name: "empty 200", want: unknown},
		{name: "server unavailable", status: http.StatusServiceUnavailable, want: unknown},
		{name: "200 carries internal error", body: "<Error><Code>InternalError</Code></Error>", want: unknown},
		{name: "non-200 success body", status: http.StatusAccepted, body: success, want: unknown},
		{name: "explicit invalid part", status: http.StatusBadRequest, body: invalidPart, want: domain.ErrMultipartUploadPartRejected},
		{name: "200 carries invalid part", body: invalidPart, want: domain.ErrMultipartUploadPartRejected},
		{name: "404 missing session", status: http.StatusNotFound, body: missingSession, want: unknown, missing: true},
		{name: "200 carries missing session", body: missingSession, want: unknown, missing: true},
		{name: "response lost", dropResponse: true, want: unknown},
		{name: "canceled after posting", cancelIO: true, body: success, want: unknown},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			receipts := multipartCompletionTestReceipts()
			var calls atomic.Int32
			client := multipartCompletionTestClient(t, func(writer http.ResponseWriter, request *http.Request) {
				// 1. 读取实际 POST，核对原会话和完整、有序的回执。
				calls.Add(1)
				query := request.URL.Query()
				if request.URL.Path != "/cloudnest/"+key || len(query) != 1 || query.Get("uploadId") != session ||
					!strings.HasPrefix(request.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
					t.Error("请求未固定使用原键和会话")
				}
				var submitted struct {
					XMLName xml.Name             `xml:"CompleteMultipartUpload"`
					Parts   []minio.CompletePart `xml:"Part"`
				}
				wantParts := []minio.CompletePart{
					{PartNumber: 1, ETag: receipts[0].ETag, ChecksumSHA256: receipts[0].ChecksumSHA256},
					{PartNumber: 2, ETag: receipts[1].ETag, ChecksumSHA256: receipts[1].ChecksumSHA256},
				}
				if err := xml.NewDecoder(request.Body).Decode(&submitted); err != nil || !reflect.DeepEqual(submitted.Parts, wantParts) {
					t.Error("请求正文没有保留分片顺序或回执", err)
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
				if fixture.cancelIO {
					cancel()
				}
				status := fixture.status
				if status == 0 {
					status = http.StatusOK
				}
				writer.Header().Set("Content-Type", "application/xml")
				if fixture.version != "" {
					writer.Header().Set("X-Amz-Version-Id", fixture.version)
				}
				writer.WriteHeader(status)
				_, _ = io.WriteString(writer, fixture.body)
			})
			// 2. 核对结果与请求次数，任何失败均无可保存的对象结果。
			result, err := client.CompleteMultipartUpload(ctx, key, session, domain.MaxSingleUploadSizeBytes+1, domain.MaxSingleUploadSizeBytes, 2, receipts)
			if !errors.Is(err, fixture.want) || errors.Is(err, domain.ErrUploadPartSessionMissing) != fixture.missing ||
				errors.Is(err, context.Canceled) != fixture.cancelIO || calls.Load() != 1 || (err != nil && strings.Contains(err.Error(), "private fixture")) {
				t.Fatal("合并结果分类或实际请求次数不符", result, err, calls.Load())
			}
			if fixture.want != nil {
				if result != nil {
					t.Fatal("失败仍交付对象元数据", result)
				}
				return
			}
			if result == nil || result.SizeBytes != domain.MaxSingleUploadSizeBytes+1 || result.StorageETag == nil || *result.StorageETag != etag ||
				(result.StorageVersionID == nil) != (fixture.version == "") || (result.StorageVersionID != nil && *result.StorageVersionID != fixture.version) {
				t.Fatal("对象元数据不符", result)
			}
		})
	}
}

func TestCompleteMultipartUploadInvalidInput(t *testing.T) {
	client := multipartCompletionTestClient(t, func(writer http.ResponseWriter, _ *http.Request) {
		t.Error("无效输入触发了合并请求")
		writer.WriteHeader(http.StatusBadRequest)
	})
	for _, fixture := range []struct {
		key, session string
		partCount    uint32
		alter        func([]domain.UploadPartReceipt)
		want         error
	}{
		{" ", "fixture", 2, nil, domain.ErrInvalidStoredObjectData},
		{"key", "\n", 2, nil, domain.ErrInvalidUploadTaskData},
		{"key", "fixture", 3, nil, domain.ErrInvalidUploadTaskData},
		{"key", "fixture", 2, func(parts []domain.UploadPartReceipt) { parts[0], parts[1] = parts[1], parts[0] }, domain.ErrInvalidUploadTaskData},
		{"key", "fixture", 2, func(parts []domain.UploadPartReceipt) { parts[1].SizeBytes++ }, domain.ErrInvalidUploadTaskData},
		{"key", "fixture", 2, func(parts []domain.UploadPartReceipt) { parts[1].ETag = "bad tag" }, domain.ErrInvalidUploadTaskData},
		{"key", "fixture", 2, func(parts []domain.UploadPartReceipt) { parts[1].ChecksumSHA256 = "" }, domain.ErrInvalidUploadTaskData},
	} {
		receipts := multipartCompletionTestReceipts()
		if fixture.alter != nil {
			fixture.alter(receipts)
		}
		result, err := client.CompleteMultipartUpload(t.Context(), fixture.key, fixture.session, domain.MaxSingleUploadSizeBytes+1, domain.MaxSingleUploadSizeBytes, fixture.partCount, receipts)
		if result != nil || !errors.Is(err, fixture.want) {
			t.Fatal("无效输入未在网络调用前被拒绝", err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if result, err := client.CompleteMultipartUpload(ctx, "key", "fixture", domain.MaxSingleUploadSizeBytes+1, domain.MaxSingleUploadSizeBytes, 2, multipartCompletionTestReceipts()); result != nil || !errors.Is(err, context.Canceled) || errors.Is(err, domain.ErrMultipartUploadCompletionUnknown) {
		t.Fatal("发起请求前取消的分类不符", err)
	}
}
