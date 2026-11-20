package storage

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func downloadObjectTestClient(t *testing.T, head http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Query().Has("location"):
			_, _ = io.WriteString(writer, "<LocationConstraint>us-east-1</LocationConstraint>")
		case request.Method == http.MethodHead && strings.Trim(request.URL.Path, "/") == "cloudnest":
			writer.WriteHeader(http.StatusOK)
		case request.Method == http.MethodHead:
			head(writer, request)
		default:
			t.Error("下载元数据检查发起了意外请求", request.Method)
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

func downloadObjectTestTarget() *domain.EntryDownloadTarget {
	return &domain.EntryDownloadTarget{
		EntryID: 13, UserID: 7, ObjectID: 23, SizeBytes: 4,
		Provider: "local_minio", Bucket: "cloudnest", ObjectKey: "users/7/objects/fixture",
	}
}

func TestCheckDownloadObject(t *testing.T) {
	// 1. 经真实 TCP 构造 HEAD 响应，覆盖版本、大小和存储错误。
	const oldVersion = "old+A/="
	for _, fixture := range []struct {
		name, requested, version, length, code string
		status                                 int
		size                                   uint64
		deleteMarker, partial                  bool
		want                                   error
	}{
		{name: "immutable key", length: "4", size: 4},
		{name: "opaque version", requested: oldVersion, version: oldVersion, length: "4", size: 4},
		{name: "explicit null with header", requested: "null", version: "null", length: "4", size: 4},
		{name: "explicit null without header", requested: "null", length: "4", size: 4},
		{name: "zero byte current version", version: "current", length: "0"},
		{name: "version changed", requested: oldVersion, version: "different", length: "4", size: 4, want: domain.ErrDownloadTargetUnavailable},
		{name: "missing explicit version header", requested: oldVersion, length: "4", size: 4, want: domain.ErrDownloadTargetUnavailable},
		{name: "size differs", length: "3", size: 4, want: domain.ErrDownloadTargetUnavailable},
		{name: "missing size", want: domain.ErrDownloadStorageUnavailable},
		{name: "partial metadata", length: "4", size: 4, status: 206, partial: true, want: domain.ErrDownloadStorageUnavailable},
		{name: "missing key", status: 404, want: domain.ErrDownloadTargetUnavailable},
		{name: "missing version", requested: oldVersion, status: 404, code: "NoSuchVersion", want: domain.ErrDownloadTargetUnavailable},
		{name: "delete marker as current", status: 404, deleteMarker: true, want: domain.ErrDownloadTargetUnavailable},
		{name: "explicit delete marker", requested: oldVersion, version: oldVersion, status: 405, deleteMarker: true, want: domain.ErrDownloadTargetUnavailable},
		{name: "missing bucket", status: 404, code: "NoSuchBucket", want: domain.ErrDownloadStorageUnavailable},
		{name: "access denied", status: 403, code: "AccessDenied", want: domain.ErrDownloadStorageUnavailable},
		{name: "bad method without marker", requested: oldVersion, status: 405, want: domain.ErrDownloadStorageUnavailable},
		{name: "storage failure", status: 503, want: domain.ErrDownloadStorageUnavailable},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			target := downloadObjectTestTarget()
			target.SizeBytes = fixture.size
			if fixture.requested != "" {
				target.StorageVersionID = new(fixture.requested)
			}
			requests := make(chan struct{}, 2)
			client := downloadObjectTestClient(t, func(writer http.ResponseWriter, request *http.Request) {
				select {
				case requests <- struct{}{}:
				default:
					t.Error("对象检查重试次数超出预期")
				}
				query := request.URL.Query()
				if request.URL.Path != "/cloudnest/"+target.ObjectKey || query.Has("versionId") != (fixture.requested != "") ||
					query.Get("versionId") != fixture.requested || request.Header.Get("Range") != "" ||
					request.Header.Get("Accept-Encoding") != "identity" || !strings.HasPrefix(request.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
					t.Error("HEAD 请求未保留签名、原始对象位置或指定版本")
				}
				writer.Header().Set("Last-Modified", "Tue, 17 Nov 2026 08:00:00 GMT")
				if fixture.version != "" {
					writer.Header().Set("x-amz-version-id", fixture.version)
				}
				if fixture.length != "" {
					writer.Header().Set("Content-Length", fixture.length)
				}
				if fixture.code != "" {
					writer.Header().Set("X-Minio-Error-Code", fixture.code)
					writer.Header().Set("X-Minio-Error-Desc", "private-storage-detail")
				}
				if fixture.deleteMarker {
					writer.Header().Set("x-amz-delete-marker", "true")
				}
				if fixture.partial {
					writer.Header().Set("Content-Range", "bytes 0-3/10")
				}
				status := fixture.status
				if status == 0 {
					status = http.StatusOK
				}
				writer.WriteHeader(status)
			})

			// 2. 检查结果和网络调用次数，错误正文保留固定分类。
			err := client.CheckDownloadObject(t.Context(), target)
			if !errors.Is(err, fixture.want) || len(requests) != 1 ||
				(err != nil && strings.Contains(err.Error(), "private-storage-detail")) {
				t.Fatal("对象检查结果、调用次数或错误保密性不符", err, len(requests))
			}
		})
	}
}

func TestCheckDownloadObjectInvalidInput(t *testing.T) {
	client := downloadObjectTestClient(t, func(writer http.ResponseWriter, _ *http.Request) {
		t.Error("无效下载目标触发了请求")
		writer.WriteHeader(http.StatusBadRequest)
	})
	for _, edit := range []func(*domain.EntryDownloadTarget){
		func(target *domain.EntryDownloadTarget) { target.ObjectID = 0 },
		func(target *domain.EntryDownloadTarget) { target.UserID = 0 },
		func(target *domain.EntryDownloadTarget) { target.Provider = "foreign" },
		func(target *domain.EntryDownloadTarget) { target.Bucket = "foreign" },
		func(target *domain.EntryDownloadTarget) { target.ObjectKey = "" },
		func(target *domain.EntryDownloadTarget) { target.ObjectKey = "key\n" },
		func(target *domain.EntryDownloadTarget) { target.StorageVersionID = new("") },
		func(target *domain.EntryDownloadTarget) { target.StorageVersionID = new("version\x00") },
		func(target *domain.EntryDownloadTarget) { target.StorageVersionID = new(strings.Repeat("v", 256)) },
	} {
		target := downloadObjectTestTarget()
		edit(target)
		if err := client.CheckDownloadObject(t.Context(), target); !errors.Is(err, domain.ErrInvalidStoredObjectData) {
			t.Fatal("无效下载目标未得到固定错误", err)
		}
	}
	if err := client.CheckDownloadObject(t.Context(), nil); !errors.Is(err, domain.ErrInvalidStoredObjectData) {
		t.Fatal("空下载目标未得到固定错误", err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := client.CheckDownloadObject(cancelled, downloadObjectTestTarget()); !errors.Is(err, context.Canceled) {
		t.Fatal("预先取消未得到取消错误", err)
	}
}

func TestCheckDownloadObjectDeadline(t *testing.T) {
	client := downloadObjectTestClient(t, func(_ http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	})
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	if err := client.CheckDownloadObject(ctx, downloadObjectTestTarget()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("检查未保留较短的调用方期限", err)
	}
}
