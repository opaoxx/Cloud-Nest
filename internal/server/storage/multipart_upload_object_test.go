package storage

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestStatMultipartUploadObject(t *testing.T) {
	const key = "users/7/objects/merge +%.bin"
	const etag = "Fixture-Object-2"
	const version = "Version+A/="
	for _, fixture := range []struct {
		name, length, etag, version, code          string
		status                                     int
		size                                       uint64
		requestID, deleteMarker, partial, cancelIO bool
		want                                       error
	}{
		{name: "versioned object", length: "16777217", etag: etag, version: version, size: 16777217},
		{name: "unversioned object", length: "16777217", etag: etag, size: 16777217},
		{name: "explicit null version and zero bytes", length: "0", etag: etag, version: "null"},
		{name: "definite missing key", status: http.StatusNotFound, code: "NoSuchKey", requestID: true, want: domain.ErrUploadObjectMissing},
		{name: "missing key with standard HEAD headers", status: http.StatusNotFound, requestID: true, want: domain.ErrUploadObjectMissing},
		{name: "generic proxy 404", status: http.StatusNotFound, want: domain.ErrUploadStorageUnavailable},
		{name: "explicit code without request ID", status: http.StatusNotFound, code: "NoSuchKey", want: domain.ErrUploadStorageUnavailable},
		{name: "missing bucket", status: http.StatusNotFound, code: "NoSuchBucket", requestID: true, want: domain.ErrUploadStorageUnavailable},
		{name: "unexpected missing version", status: http.StatusNotFound, code: "NoSuchVersion", requestID: true, want: domain.ErrUploadStorageUnavailable},
		{name: "access denied", status: http.StatusForbidden, code: "AccessDenied", requestID: true, want: domain.ErrUploadStorageUnavailable},
		{name: "missing code with wrong status", status: http.StatusForbidden, code: "NoSuchKey", requestID: true, want: domain.ErrUploadStorageUnavailable},
		{name: "storage failure", status: http.StatusServiceUnavailable, want: domain.ErrUploadStorageUnavailable},
		{name: "delete marker masks object", status: http.StatusNotFound, code: "NoSuchKey", requestID: true, deleteMarker: true, want: domain.ErrUploadStorageUnavailable},
		{name: "404 carries a version", status: http.StatusNotFound, code: "NoSuchKey", requestID: true, version: version, want: domain.ErrUploadStorageUnavailable},
		{name: "nominal success with delete marker", length: "4", etag: etag, deleteMarker: true, want: domain.ErrUploadStorageUnavailable},
		{name: "missing length", etag: etag, want: domain.ErrUploadStorageUnavailable},
		{name: "partial metadata", status: http.StatusPartialContent, length: "4", etag: etag, partial: true, want: domain.ErrUploadStorageUnavailable},
		{name: "missing ETag", length: "4", want: domain.ErrUploadStorageUnavailable},
		{name: "blank ETag", length: "4", etag: " ", want: domain.ErrUploadStorageUnavailable},
		{name: "oversized ETag", length: "4", etag: strings.Repeat("e", 256), want: domain.ErrUploadStorageUnavailable},
		{name: "oversized version", length: "4", etag: etag, version: strings.Repeat("v", 256), want: domain.ErrUploadStorageUnavailable},
		{name: "cancellation wins over missing", status: http.StatusNotFound, code: "NoSuchKey", requestID: true, cancelIO: true, want: context.Canceled},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 在回环地址验证 SDK 发起的是同一个键的无正文 HEAD 查询。
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var requests atomic.Int32
			client := downloadObjectTestClient(t, func(writer http.ResponseWriter, request *http.Request) {
				requests.Add(1)
				if request.Method != http.MethodHead || request.URL.Path != "/cloudnest/"+key || request.URL.RawQuery != "" ||
					request.Header.Get("Range") != "" || request.Header.Get("Accept-Encoding") != "identity" ||
					!strings.HasPrefix(request.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
					t.Error("HEAD 未使用原对象键或只读请求选项")
				}
				writer.Header().Set("Last-Modified", "Tue, 17 Nov 2026 08:00:00 GMT")
				if fixture.length != "" {
					writer.Header().Set("Content-Length", fixture.length)
				}
				if fixture.etag != "" {
					writer.Header().Set("ETag", "\""+fixture.etag+"\"")
				}
				if fixture.version != "" {
					writer.Header().Set("x-amz-version-id", fixture.version)
				}
				if fixture.requestID {
					writer.Header().Set("x-amz-request-id", "fixture-request")
				}
				if fixture.code != "" {
					writer.Header().Set("X-Minio-Error-Code", fixture.code)
					writer.Header().Set("X-Minio-Error-Desc", "private-fixture-detail")
				}
				if fixture.deleteMarker {
					writer.Header().Set("x-amz-delete-marker", "true")
				}
				if fixture.partial {
					writer.Header().Set("Content-Range", "bytes 0-3/8")
				}
				if fixture.cancelIO {
					cancel()
				}
				status := fixture.status
				if status == 0 {
					status = http.StatusOK
				}
				writer.WriteHeader(status)
			})

			// 2. 只交付完整元数据，检查缺失、故障与取消的类别。
			result, err := client.StatMultipartUploadObject(ctx, key)
			if !errors.Is(err, fixture.want) || requests.Load() != 1 || (err != nil && strings.Contains(err.Error(), "private-fixture-detail")) {
				t.Fatal("查询类别、调用次数或错误文本异常", err, requests.Load())
			}
			if fixture.want != nil {
				if result != nil {
					t.Fatal("失败时仍交付了元数据", result)
				}
				return
			}
			if result == nil || result.SizeBytes != fixture.size || result.StorageETag == nil || *result.StorageETag != fixture.etag ||
				(result.StorageVersionID == nil) != (fixture.version == "") || (result.StorageVersionID != nil && *result.StorageVersionID != fixture.version) {
				t.Fatal("元数据未保留实际大小、ETag 或版本", result)
			}
		})
	}
}

func TestStatMultipartUploadObjectInvalidInput(t *testing.T) {
	client := downloadObjectTestClient(t, func(writer http.ResponseWriter, _ *http.Request) {
		t.Error("非法对象键或已取消的查询发起了请求")
		writer.WriteHeader(http.StatusBadRequest)
	})
	for _, key := range []string{"", "  ", "key\n", "对象", strings.Repeat("k", 256)} {
		if result, err := client.StatMultipartUploadObject(t.Context(), key); result != nil || !errors.Is(err, domain.ErrInvalidStoredObjectData) {
			t.Fatal("非法对象键未被拒绝", err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if result, err := client.StatMultipartUploadObject(ctx, "valid-key"); result != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("没有保留提前取消", err)
	}
}
