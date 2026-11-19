package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func verificationObjectClient(t *testing.T, get http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Query().Has("location"):
			_, _ = io.WriteString(writer, "<LocationConstraint>us-east-1</LocationConstraint>")
		case request.Method == http.MethodHead && strings.Trim(request.URL.Path, "/") == "cloudnest":
			writer.WriteHeader(http.StatusOK)
		case request.Method == http.MethodGet:
			get(writer, request)
		default:
			t.Error("读取对象使用了意外请求方法", request.Method)
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

func verificationObjectFixture() *domain.StoredObject {
	return &domain.StoredObject{
		ID: 23, OwnerUserID: 7, Provider: "local_minio", Bucket: "cloudnest",
		ObjectKey: "users/7/objects/fixture", Status: domain.StoredObjectStatusVerifying,
	}
}

func verificationObjectHeaders(writer http.ResponseWriter, version string) {
	writer.Header().Set("Last-Modified", "Tue, 17 Nov 2026 08:00:00 GMT")
	writer.Header().Set("ETag", `"stored-etag"`)
	if version != "" {
		writer.Header().Set("x-amz-version-id", version)
	}
}

func TestOpenUploadVerificationObjectVersion(t *testing.T) {
	for _, fixture := range []struct{ name, version, returnedVersion string }{
		{"no requested version", "", "latest"},
		{"explicit null header", "null", "null"},
		{"implicit null header", "null", ""},
		{"explicit opaque version", "old+A/=", "old+A/="},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			version := fixture.version
			object := verificationObjectFixture()
			if version != "" {
				object.StorageVersionID = &version
			}
			object.StorageETag = new("stored-etag")
			client := verificationObjectClient(t, func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/cloudnest/"+object.ObjectKey || request.URL.Query().Get("versionId") != version ||
					request.URL.Query().Has("versionId") != (version != "") || request.Header.Get("Range") != "" ||
					request.Header.Get("Accept-Encoding") != "identity" {
					t.Error("校验读取未定位指定版本或原始字节")
				}
				verificationObjectHeaders(writer, fixture.returnedVersion)
				writer.Header().Set("Content-Encoding", "gzip")
				_, _ = io.WriteString(writer, "raw\x00\xff")
			})
			stream, err := client.OpenUploadVerificationObject(t.Context(), object)
			if err != nil {
				t.Fatal(err)
			}
			data, readError := io.ReadAll(stream)
			closeError := stream.Close()
			if string(data) != "raw\x00\xff" || readError != nil || closeError != nil {
				t.Fatal("读回字节或关闭错误", readError, closeError)
			}
		})
	}
}

func TestOpenUploadVerificationObjectFailures(t *testing.T) {
	for _, fixture := range []struct {
		name   string
		status int
		code   string
		want   error
	}{
		{"absent object", 404, "NoSuchKey", domain.ErrUploadObjectMissing},
		{"absent version", 404, "NoSuchVersion", domain.ErrUploadObjectMissing},
		{"absent bucket", 404, "NoSuchBucket", domain.ErrUploadStorageUnavailable},
		{"permission", 403, "AccessDenied", domain.ErrUploadStorageUnavailable},
		{"storage failure", 500, "InternalError", domain.ErrUploadStorageUnavailable},
		{"wrong version", 200, "", domain.ErrInvalidStoredObjectData},
		{"wrong ETag", 200, "", domain.ErrInvalidStoredObjectData},
		{"partial response", 206, "", domain.ErrInvalidStoredObjectData},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			object := verificationObjectFixture()
			object.StorageVersionID = new("saved-version")
			object.StorageETag = new("stored-etag")
			client := verificationObjectClient(t, func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Query().Get("versionId") != "saved-version" {
					t.Error("失败场景悄悄切换了版本")
				}
				if fixture.code != "" {
					writer.WriteHeader(fixture.status)
					_, _ = fmt.Fprintf(writer, "<Error><Code>%s</Code><Message>private-origin-detail</Message></Error>", fixture.code)
					return
				}
				verificationObjectHeaders(writer, "saved-version")
				switch fixture.name {
				case "wrong version":
					writer.Header().Set("x-amz-version-id", "another-version")
				case "wrong ETag":
					writer.Header().Set("ETag", `"another-etag"`)
				case "partial response":
					writer.Header().Set("Content-Range", "bytes 0-0/3")
				}
				writer.WriteHeader(fixture.status)
				_, _ = io.WriteString(writer, "a")
			})
			stream, err := client.OpenUploadVerificationObject(t.Context(), object)
			if stream != nil || !errors.Is(err, fixture.want) || strings.Contains(err.Error(), "private-origin-detail") {
				t.Fatal("打开失败类型或结果不符", stream, err)
			}
		})
	}
}

func TestOpenUploadVerificationObjectRejectsInvalidInput(t *testing.T) {
	client := verificationObjectClient(t, func(writer http.ResponseWriter, request *http.Request) {
		t.Error("无效输入触发了 GET")
		writer.WriteHeader(http.StatusBadRequest)
	})
	for _, name := range []string{"missing record", "wrong status", "wrong bucket", "wrong provider", "empty key", "control key", "empty version", "control version", "long version"} {
		t.Run(name, func(t *testing.T) {
			object := verificationObjectFixture()
			switch name {
			case "missing record":
				object = nil
			case "wrong status":
				object.Status = domain.StoredObjectStatusStaging
			case "wrong bucket":
				object.Bucket = "another-bucket"
			case "wrong provider":
				object.Provider = "another-provider"
			case "empty key":
				object.ObjectKey = ""
			case "control key":
				object.ObjectKey = "bad\nkey"
			case "empty version":
				object.StorageVersionID = new("")
			case "control version":
				object.StorageVersionID = new("bad\nversion")
			case "long version":
				object.StorageVersionID = new(strings.Repeat("a", 256))
			}
			if stream, err := client.OpenUploadVerificationObject(t.Context(), object); stream != nil || !errors.Is(err, domain.ErrInvalidStoredObjectData) {
				t.Fatal("无效对象信息被接受", stream, err)
			}
		})
	}
}

func TestOpenUploadVerificationObjectStreaming(t *testing.T) {
	t.Run("deadline before headers", func(t *testing.T) {
		client := verificationObjectClient(t, func(_ http.ResponseWriter, request *http.Request) {
			<-request.Context().Done()
		})
		ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
		defer cancel()
		stream, err := client.OpenUploadVerificationObject(ctx, verificationObjectFixture())
		if stream != nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("打开流未保留截止时间错误", stream, err)
		}
	})
	t.Run("headers before body and cancellation", func(t *testing.T) {
		finished := make(chan struct{})
		client := verificationObjectClient(t, func(writer http.ResponseWriter, request *http.Request) {
			defer close(finished)
			verificationObjectHeaders(writer, "")
			writer.Header().Set("Content-Length", "3")
			writer.(http.Flusher).Flush()
			<-request.Context().Done()
		})
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		stream, err := client.OpenUploadVerificationObject(ctx, verificationObjectFixture())
		if err != nil {
			t.Fatal("打开流不应等待全部正文", err)
		}
		defer stream.Close()
		cancel()
		if _, err := stream.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
			t.Fatal("读取未保留调用取消", err)
		}
		select {
		case <-finished:
		case <-time.After(3 * time.Second):
			t.Fatal("服务端流未随取消结束")
		}
	})
	t.Run("read failure", func(t *testing.T) {
		client := verificationObjectClient(t, func(writer http.ResponseWriter, _ *http.Request) {
			verificationObjectHeaders(writer, "")
			writer.Header().Set("Content-Length", "4")
			_, _ = io.WriteString(writer, "ab")
		})
		stream, err := client.OpenUploadVerificationObject(t.Context(), verificationObjectFixture())
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		data, err := io.ReadAll(stream)
		if string(data) != "ab" || !errors.Is(err, domain.ErrUploadStorageUnavailable) {
			t.Fatal("正文中断被误判为正常 EOF", data, err)
		}
	})
}
