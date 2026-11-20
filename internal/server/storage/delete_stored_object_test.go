package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func deleteStoredObjectTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Query().Has("location"):
			_, _ = io.WriteString(writer, "<LocationConstraint>us-east-1</LocationConstraint>")
		case request.Method == http.MethodHead && strings.Trim(request.URL.Path, "/") == "cloudnest":
			writer.WriteHeader(http.StatusOK)
		default:
			handler(writer, request)
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

func TestDeleteStoredObjectVersion(t *testing.T) {
	for _, fixture := range []struct {
		name    string
		version *string
		want    string
	}{
		{"unversioned object", nil, "null"},
		{"explicit null", new("null"), "null"},
		{"opaque version", new("old+A/="), "old+A/="},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			object := verificationObjectFixture()
			object.Status = domain.StoredObjectStatusDeleting
			object.StorageVersionID = fixture.version
			var reads, deletes atomic.Int32
			client := deleteStoredObjectTestClient(t, func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/cloudnest/"+object.ObjectKey {
					t.Error("删除请求使用了错误的对象位置")
				}
				if request.Method == http.MethodHead && fixture.version == nil {
					reads.Add(1)
					verificationObjectHeaders(writer, "")
					writer.Header().Set("Content-Length", "0")
					return
				}
				deletes.Add(1)
				if request.Method != http.MethodDelete || request.URL.Query().Get("versionId") != fixture.want ||
					request.Header.Get("x-amz-bypass-governance-retention") != "" {
					t.Error("删除请求没有精确使用目标版本")
				}
				writer.WriteHeader(http.StatusNoContent)
			})
			if err := client.DeleteStoredObject(t.Context(), object); err != nil || deletes.Load() != 1 ||
				(reads.Load() == 1) != (fixture.version == nil) {
				t.Fatal("版本核对或删除次数不符", reads.Load(), deletes.Load(), err)
			}
		})
	}
}

func TestDeleteStoredObjectFailures(t *testing.T) {
	for _, fixture := range []struct {
		name, code string
		status     int
		want       error
	}{
		{"missing object", "NoSuchKey", 404, nil},
		{"missing version", "NoSuchVersion", 404, nil},
		{"missing bucket", "NoSuchBucket", 404, domain.ErrStoredObjectDeleteUnavailable},
		{"unstructured 404", "", 404, domain.ErrStoredObjectDeleteUnavailable},
		{"permission denied", "AccessDenied", 403, domain.ErrStoredObjectDeleteUnavailable},
		{"storage failure", "InternalError", 500, domain.ErrStoredObjectDeleteUnavailable},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			object := verificationObjectFixture()
			object.Status = domain.StoredObjectStatusDeleting
			object.StorageVersionID = new("saved-version")
			client := deleteStoredObjectTestClient(t, func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodDelete || request.URL.Query().Get("versionId") != "saved-version" {
					t.Error("错误场景切换了删除目标")
				}
				writer.WriteHeader(fixture.status)
				if fixture.code != "" {
					_, _ = fmt.Fprintf(writer, "<Error><Code>%s</Code><RequestId>fixture-request</RequestId><Message>private-origin-detail</Message></Error>", fixture.code)
				}
			})
			err := client.DeleteStoredObject(t.Context(), object)
			if !errors.Is(err, fixture.want) || (err != nil && strings.Contains(err.Error(), "private-origin-detail")) {
				t.Fatal("删除错误未按预期交付", err)
			}
		})
	}
}

func TestDeleteStoredObjectRejectsUnverifiedTargets(t *testing.T) {
	client := deleteStoredObjectTestClient(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodHead {
			t.Error("未确认版本时不应发出删除请求")
		}
		verificationObjectHeaders(writer, "named-version")
		writer.Header().Set("Content-Length", "0")
	})
	object := verificationObjectFixture()
	if err := client.DeleteStoredObject(t.Context(), object); !errors.Is(err, domain.ErrInvalidStoredObjectData) {
		t.Fatal("未提交删除意图的对象应被拒绝", err)
	}
	object.Status = domain.StoredObjectStatusDeleting
	if err := client.DeleteStoredObject(t.Context(), object); !errors.Is(err, domain.ErrInvalidStoredObjectData) {
		t.Fatal("缺少版本时应拒绝命名版本", err)
	}
	for _, change := range []func(*domain.StoredObject){
		func(target *domain.StoredObject) { target.Bucket = "foreign-bucket" },
		func(target *domain.StoredObject) { target.Provider = "foreign" },
		func(target *domain.StoredObject) { target.ObjectKey = "invalid\nkey" },
		func(target *domain.StoredObject) { target.StorageVersionID = new("") },
	} {
		target := *object
		change(&target)
		if err := client.DeleteStoredObject(t.Context(), &target); !errors.Is(err, domain.ErrInvalidStoredObjectData) {
			t.Fatal("无效存储位置或版本应被拒绝", err)
		}
	}
}

func TestDeleteStoredObjectCancellation(t *testing.T) {
	client := deleteStoredObjectTestClient(t, func(writer http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	})
	object := verificationObjectFixture()
	object.Status = domain.StoredObjectStatusDeleting
	object.StorageVersionID = new("saved-version")
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := client.DeleteStoredObject(ctx, object); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("删除未保留调用方的超时原因", err)
	}
}
