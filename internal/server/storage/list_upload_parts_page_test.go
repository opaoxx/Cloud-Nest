package storage

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const (
	listPartsFixtureKey = "users/7/objects/list-parts+fixture"
	listPartsFixtureID  = "session+A/="
	listPartsDigest     = "TgdAhWK+24tgzgXB3s/jrRa3IjCWfeAfZAt+Rym0n84="
)

func listPartsTestClient(t *testing.T, readPage http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Query().Has("location"):
			_, _ = io.WriteString(response, "<LocationConstraint>us-east-1</LocationConstraint>")
		case request.Method == http.MethodHead && strings.Trim(request.URL.Path, "/") == "cloudnest":
			response.WriteHeader(http.StatusOK)
		case request.Method == http.MethodGet && request.URL.Path == "/cloudnest/"+listPartsFixtureKey:
			readPage(response, request)
		default:
			t.Error("出现计划以外的存储操作", request.Method)
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

func TestListUploadPartsPageReadsReceipts(t *testing.T) {
	first := minio.ObjectPart{PartNumber: 3, Size: 4096, ETag: `"Ab.C:_-9"`, ChecksumSHA256: listPartsDigest}
	last := minio.ObjectPart{PartNumber: 7, Size: 0, ETag: "Other", ChecksumSHA256: listPartsDigest}
	for _, test := range []struct {
		name          string
		marker, next  uint32
		limit         int
		truncated     bool
		observedParts []minio.ObjectPart
		want          domain.UploadPartReceiptPage
	}{
		{name: "empty", limit: 1000, want: domain.UploadPartReceiptPage{Parts: []domain.UploadPartReceipt{}}},
		{name: "first page with gap", next: 3, limit: 1, truncated: true, observedParts: []minio.ObjectPart{first},
			want: domain.UploadPartReceiptPage{NextMarker: 3, Parts: []domain.UploadPartReceipt{
				{PartNumber: 3, SizeBytes: 4096, ETag: "Ab.C:_-9", ChecksumSHA256: listPartsDigest},
			}}},
		{name: "terminal page with actual zero size", marker: 3, next: 7, limit: 1, observedParts: []minio.ObjectPart{last},
			want: domain.UploadPartReceiptPage{Parts: []domain.UploadPartReceipt{{PartNumber: 7, ETag: "Other", ChecksumSHA256: listPartsDigest}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			client := listPartsTestClient(t, func(response http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				query := request.URL.Query()
				if query.Get("uploadId") != listPartsFixtureID || query.Get("part-number-marker") != strconv.FormatUint(uint64(test.marker), 10) ||
					query.Get("max-parts") != strconv.Itoa(test.limit) || !strings.HasPrefix(request.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
					t.Error("未使用预期的会话、分页参数及内部存储签名")
				}
				_ = xml.NewEncoder(response).Encode(struct {
					XMLName xml.Name `xml:"ListPartsResult"`
					minio.ListObjectPartsResult
				}{ListObjectPartsResult: minio.ListObjectPartsResult{Bucket: "cloudnest", Key: listPartsFixtureKey, UploadID: listPartsFixtureID,
					PartNumberMarker: int(test.marker), NextPartNumberMarker: int(test.next), IsTruncated: test.truncated, ObjectParts: test.observedParts}})
			})
			page, err := client.ListUploadPartsPage(t.Context(), listPartsFixtureKey, listPartsFixtureID, test.marker, test.limit)
			if err != nil || !reflect.DeepEqual(page, &test.want) || calls.Load() != 1 {
				t.Fatal("回执、实际片长或下一页标记不符", page, err, calls.Load())
			}
		})
	}
}

func TestListUploadPartsPageRejectsInvalidResult(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*minio.ListObjectPartsResult)
	}{
		{"different bucket", func(page *minio.ListObjectPartsResult) { page.Bucket = "private-bucket" }},
		{"different key", func(page *minio.ListObjectPartsResult) { page.Key = "private-key" }},
		{"different session", func(page *minio.ListObjectPartsResult) { page.UploadID = "private-session" }},
		{"different marker", func(page *minio.ListObjectPartsResult) { page.PartNumberMarker = 0 }},
		{"too many parts", func(page *minio.ListObjectPartsResult) {
			page.ObjectParts = append(page.ObjectParts, page.ObjectParts...)
		}},
		{"part at requested marker", func(page *minio.ListObjectPartsResult) { page.ObjectParts[0].PartNumber = 1 }},
		{"duplicate part", func(page *minio.ListObjectPartsResult) { page.ObjectParts[1].PartNumber = 2 }},
		{"backward order", func(page *minio.ListObjectPartsResult) { page.ObjectParts[0].PartNumber = 4 }},
		{"part above limit", func(page *minio.ListObjectPartsResult) {
			page.ObjectParts[1].PartNumber = domain.MaxUploadPartCount + 1
		}},
		{"negative size", func(page *minio.ListObjectPartsResult) { page.ObjectParts[1].Size = -1 }},
		{"invalid ETag", func(page *minio.ListObjectPartsResult) { page.ObjectParts[1].ETag = `""quoted""` }},
		{"missing checksum", func(page *minio.ListObjectPartsResult) { page.ObjectParts[1].ChecksumSHA256 = "" }},
		{"invalid checksum", func(page *minio.ListObjectPartsResult) {
			page.ObjectParts[1].ChecksumSHA256 = strings.Repeat("A", 42) + "B="
		}},
		{"missing next marker", func(page *minio.ListObjectPartsResult) { page.NextPartNumberMarker = 0 }},
		{"nonadvancing next marker", func(page *minio.ListObjectPartsResult) { page.NextPartNumberMarker = page.PartNumberMarker }},
		{"marker would skip parts", func(page *minio.ListObjectPartsResult) { page.NextPartNumberMarker = 4 }},
		{"empty truncated page", func(page *minio.ListObjectPartsResult) { page.ObjectParts = nil }},
		{"truncated at maximum part", func(page *minio.ListObjectPartsResult) {
			page.ObjectParts[1].PartNumber = domain.MaxUploadPartCount
			page.NextPartNumberMarker = domain.MaxUploadPartCount
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := minio.ListObjectPartsResult{Bucket: "cloudnest", Key: listPartsFixtureKey, UploadID: listPartsFixtureID,
				PartNumberMarker: 1, NextPartNumberMarker: 3, IsTruncated: true, ObjectParts: []minio.ObjectPart{
					{PartNumber: 2, Size: 1, ETag: "ab", ChecksumSHA256: listPartsDigest},
					{PartNumber: 3, Size: 1, ETag: "cd", ChecksumSHA256: listPartsDigest},
				}}
			test.change(&fixture)
			client := listPartsTestClient(t, func(response http.ResponseWriter, _ *http.Request) {
				_ = xml.NewEncoder(response).Encode(fixture)
			})
			if page, err := client.ListUploadPartsPage(t.Context(), listPartsFixtureKey, listPartsFixtureID, 1, 2); page != nil || err != domain.ErrUploadStorageUnavailable {
				t.Fatal("异常页未整体返回固定存储错误")
			}
		})
	}
}

func TestListUploadPartsPageStorageErrors(t *testing.T) {
	for _, test := range []struct {
		status int
		body   string
	}{
		{http.StatusOK, "<ListPartsResult><Private>private-body</"},
		{http.StatusNotFound, "<Error><Code>NoSuchUpload</Code><Message>private-body</Message></Error>"},
		{http.StatusServiceUnavailable, "<Error><Code>ServiceUnavailable</Code><Message>private-body</Message></Error>"},
	} {
		client := listPartsTestClient(t, func(response http.ResponseWriter, _ *http.Request) {
			response.WriteHeader(test.status)
			_, _ = io.WriteString(response, test.body)
		})
		if page, err := client.ListUploadPartsPage(t.Context(), listPartsFixtureKey, listPartsFixtureID, 0, 10); page != nil || err != domain.ErrUploadStorageUnavailable {
			t.Fatal("底层存储响应未按固定错误处理", test.status)
		}
	}
}

func TestListUploadPartsPageInputsAndCancellation(t *testing.T) {
	var calls atomic.Int32
	requestStarted := make(chan struct{})
	client := listPartsTestClient(t, func(response http.ResponseWriter, request *http.Request) {
		if calls.Add(1) == 1 {
			close(requestStarted)
		}
		select {
		case <-request.Context().Done():
		case <-time.After(2 * time.Second):
			t.Error("只读请求未及时取消")
			response.WriteHeader(http.StatusGatewayTimeout)
		}
	})
	for _, test := range []struct {
		key, session string
		marker       uint32
		limit        int
		want         error
	}{
		{"", listPartsFixtureID, 0, 1, domain.ErrInvalidStoredObjectData},
		{listPartsFixtureKey, "invalid\n", 0, 1, domain.ErrInvalidUploadTaskData},
		{listPartsFixtureKey, listPartsFixtureID, 0, 0, nil},
		{listPartsFixtureKey, listPartsFixtureID, 0, 1001, nil},
		{listPartsFixtureKey, listPartsFixtureID, domain.MaxUploadPartCount + 1, 1, nil},
	} {
		page, err := client.ListUploadPartsPage(t.Context(), test.key, test.session, test.marker, test.limit)
		if page != nil || err == nil || (test.want != nil && !errors.Is(err, test.want)) {
			t.Fatal("无效输入未拒绝")
		}
	}
	expired, releaseExpired := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer releaseExpired()
	if page, err := client.ListUploadPartsPage(expired, listPartsFixtureKey, listPartsFixtureID, 0, 1); page != nil ||
		!errors.Is(err, context.DeadlineExceeded) || calls.Load() != 0 {
		t.Fatal("无效参数或到期上下文触发了存储请求", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		select {
		case <-requestStarted:
			cancel()
		case <-ctx.Done():
		}
	}()
	if page, err := client.ListUploadPartsPage(ctx, listPartsFixtureKey, listPartsFixtureID, 0, 1); page != nil ||
		!errors.Is(err, context.Canceled) || calls.Load() != 1 {
		t.Fatal("在途取消未按上下文退出", err)
	}
}
