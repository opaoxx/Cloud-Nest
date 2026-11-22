package storage

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const listingObjectKey = "users/7/objects/4b351d22-0cab-4a8d-89cc-9fe9d0641e25"

func multipartSessionListingTestClient(t *testing.T, readPage http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Query().Has("location"):
			_, _ = io.WriteString(response, "<LocationConstraint>us-east-1</LocationConstraint>")
		case request.Method == http.MethodHead && strings.Trim(request.URL.Path, "/") == "cloudnest":
			response.WriteHeader(http.StatusOK)
		case request.Method == http.MethodGet && strings.Trim(request.URL.Path, "/") == "cloudnest" && request.URL.Query().Has("uploads"):
			readPage(response, request)
		default:
			t.Error("出现预期之外的存储操作", request.Method, request.URL.Path)
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

func TestListMultipartUploadSessionsPageReadsBoundedPages(t *testing.T) {
	timestamp := time.Date(2026, 11, 20, 9, 0, 0, 0, time.FixedZone("fixture", 8*60*60))
	const first, second = "z-original", "A +/="
	client := multipartSessionListingTestClient(t, func(response http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		if query.Get("prefix") != listingObjectKey || query.Get("max-uploads") != "1" || query.Get("delimiter") != "" {
			t.Error("列表未限制在完整对象键及单页上限")
		}
		page := minio.ListMultipartUploadsResult{Bucket: "cloudnest", Prefix: url.QueryEscape(listingObjectKey), EncodingType: "url"}
		entry := minio.ObjectMultipartInfo{Key: url.QueryEscape(listingObjectKey), UploadID: first, Initiated: timestamp}
		switch query.Get("upload-id-marker") {
		case "":
			if query.Has("key-marker") {
				t.Error("第一页带有对象标记")
			}
			page.IsTruncated, page.NextUploadIDMarker = true, first
		case first:
			if query.Get("key-marker") != listingObjectKey {
				t.Error("第二页未保持同一对象键")
			}
			page.KeyMarker = url.QueryEscape(listingObjectKey)
			entry.UploadID = second
		default:
			t.Error("会话游标被修改")
		}
		page.Uploads = []minio.ObjectMultipartInfo{entry}
		if err := xml.NewEncoder(response).Encode(page); err != nil {
			t.Error(err)
		}
	})
	for _, fixture := range []struct{ marker, uploadID, next string }{{"", first, first}, {first, second, ""}} {
		page, err := client.ListMultipartUploadSessionsPage(t.Context(), listingObjectKey, fixture.marker, 1)
		want := &domain.MultipartUploadSessionPage{NextUploadID: fixture.next,
			Sessions: []domain.MultipartUploadSession{{ObjectKey: listingObjectKey, UploadID: fixture.uploadID, InitiatedAt: timestamp.UTC()}}}
		if err != nil || !reflect.DeepEqual(page, want) {
			t.Fatal("会话列表、原会话 ID 或游标不符", page, err)
		}
	}
}

func TestListMultipartUploadSessionsPageRejectsInvalidResults(t *testing.T) {
	for _, fixture := range []struct {
		name string
		edit func(*minio.ListMultipartUploadsResult)
	}{
		{"wrong bucket", func(page *minio.ListMultipartUploadsResult) { page.Bucket = "other" }},
		{"wrong prefix", func(page *minio.ListMultipartUploadsResult) { page.Prefix = "users/" }},
		{"other object", func(page *minio.ListMultipartUploadsResult) { page.Uploads[0].Key += "-other" }},
		{"wrong echoed marker", func(page *minio.ListMultipartUploadsResult) { page.KeyMarker = "users/" }},
		{"next marker changed key", func(page *minio.ListMultipartUploadsResult) { page.NextKeyMarker = "users/8/" }},
		{"duplicate session", func(page *minio.ListMultipartUploadsResult) { page.Uploads = append(page.Uploads, page.Uploads[0]) }},
		{"current marker repeated", func(page *minio.ListMultipartUploadsResult) { page.Uploads[0].UploadID = "previous" }},
		{"missing initiation time", func(page *minio.ListMultipartUploadsResult) { page.Uploads[0].Initiated = time.Time{} }},
		{"truncated empty page", func(page *minio.ListMultipartUploadsResult) { page.Uploads, page.IsTruncated = nil, true }},
		{"truncated without marker", func(page *minio.ListMultipartUploadsResult) { page.IsTruncated = true }},
		{"truncated with unrelated marker", func(page *minio.ListMultipartUploadsResult) {
			page.IsTruncated, page.NextUploadIDMarker = true, "other"
		}},
		{"over limit", func(page *minio.ListMultipartUploadsResult) {
			page.Uploads = append(page.Uploads, page.Uploads[0], page.Uploads[0])
		}},
		{"backward time", func(page *minio.ListMultipartUploadsResult) {
			entry := page.Uploads[0]
			entry.UploadID, entry.Initiated = "earlier", entry.Initiated.Add(-time.Second)
			page.Uploads = append(page.Uploads, entry)
		}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			client := multipartSessionListingTestClient(t, func(response http.ResponseWriter, _ *http.Request) {
				page := minio.ListMultipartUploadsResult{Bucket: "cloudnest", Prefix: listingObjectKey, KeyMarker: listingObjectKey,
					Uploads: []minio.ObjectMultipartInfo{{Key: listingObjectKey, UploadID: "fixture", Initiated: time.Now()}}}
				fixture.edit(&page)
				if err := xml.NewEncoder(response).Encode(page); err != nil {
					t.Error(err)
				}
			})
			if page, err := client.ListMultipartUploadSessionsPage(t.Context(), listingObjectKey, "previous", 2); page != nil || err != domain.ErrUploadStorageUnavailable {
				t.Fatal("异常列表复验失败", page, err)
			}
		})
	}
}

func TestListMultipartUploadSessionsPageInputsAndErrors(t *testing.T) {
	client := multipartSessionListingTestClient(t, func(response http.ResponseWriter, _ *http.Request) {
		t.Error("错误输入不应访问存储")
		response.WriteHeader(http.StatusBadRequest)
	})
	for _, key := range []string{"", "users/", "users/07/objects/4b351d22-0cab-4a8d-89cc-9fe9d0641e25", listingObjectKey + "/child", "fixture/file.bin"} {
		if _, err := client.ListMultipartUploadSessionsPage(t.Context(), key, "", 1); err != domain.ErrInvalidStoredObjectData {
			t.Fatal("接受了不属于应用的对象键", key, err)
		}
	}
	for _, limit := range []int{0, 1001} {
		if _, err := client.ListMultipartUploadSessionsPage(t.Context(), listingObjectKey, "", limit); err == nil {
			t.Fatal("接受了无效页大小", limit)
		}
	}
	if _, err := client.ListMultipartUploadSessionsPage(t.Context(), listingObjectKey, "invalid\nmarker", 1); err != domain.ErrInvalidUploadTaskData {
		t.Fatal("接受了无效的会话游标", err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := client.ListMultipartUploadSessionsPage(canceled, listingObjectKey, "", 1); !errors.Is(err, context.Canceled) {
		t.Fatal("未交付取消原因", err)
	}
	failed := multipartSessionListingTestClient(t, func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(response, "<Error><Code>AccessDenied</Code><Message>fixture private error</Message></Error>")
	})
	if page, err := failed.ListMultipartUploadSessionsPage(t.Context(), listingObjectKey, "", 1); page != nil || err != domain.ErrUploadStorageUnavailable {
		t.Fatal("存储错误未转换为统一类别", page, err)
	}
}
