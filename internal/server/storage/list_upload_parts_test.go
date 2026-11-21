package storage

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func writeUploadPartsPageFixture(t *testing.T, response http.ResponseWriter, marker, next int, parts []minio.ObjectPart) {
	t.Helper()
	page := minio.ListObjectPartsResult{
		Bucket: "cloudnest", Key: listPartsFixtureKey, UploadID: listPartsFixtureID,
		PartNumberMarker: marker, IsTruncated: next != 0, NextPartNumberMarker: next, ObjectParts: parts,
	}
	if err := xml.NewEncoder(response).EncodeElement(page, xml.StartElement{Name: xml.Name{Local: "ListPartsResult"}}); err != nil {
		t.Error("写出测试页失败", err)
	}
}

func TestListUploadPartsCollectsFullPages(t *testing.T) {
	const totalParts = maxUploadPartsPageSize + 1
	var calls atomic.Int32
	client := listPartsTestClient(t, func(response http.ResponseWriter, request *http.Request) {
		// 1. 用默认尺寸构造满页及续页，校验两次请求来自同一上传会话。
		call := calls.Add(1)
		query := request.URL.Query()
		if query.Get("uploadId") != listPartsFixtureID || query.Get("max-parts") != strconv.Itoa(maxUploadPartsPageSize) {
			t.Error("跨页读取改变了会话或每页上限")
		}
		switch call {
		case 1:
			if query.Get("part-number-marker") != "0" {
				t.Error("首轮没有从标记零开始")
			}
			first := make([]minio.ObjectPart, maxUploadPartsPageSize)
			for index := range first {
				number := index + 1
				first[index] = minio.ObjectPart{PartNumber: number, Size: int64(number), ETag: "fixture", ChecksumSHA256: listPartsDigest}
			}
			writeUploadPartsPageFixture(t, response, 0, maxUploadPartsPageSize, first)
		case 2:
			if query.Get("part-number-marker") != strconv.Itoa(maxUploadPartsPageSize) {
				t.Error("续页没有使用前一页返回的标记")
			}
			tail := minio.ObjectPart{PartNumber: totalParts, Size: totalParts, ETag: "fixture", ChecksumSHA256: listPartsDigest}
			writeUploadPartsPageFixture(t, response, maxUploadPartsPageSize, 0, []minio.ObjectPart{tail})
		default:
			t.Error("末页之后仍访问了存储")
			response.WriteHeader(http.StatusBadRequest)
		}
	})

	// 2. 核对全部 1001 项的顺序及原回执。
	receipts, err := client.ListUploadParts(t.Context(), listPartsFixtureKey, listPartsFixtureID, maxUploadPartsPageSize)
	if err != nil || len(receipts) != totalParts || calls.Load() != 2 {
		t.Fatal("未完整读到超过一个满页的回执", len(receipts), calls.Load(), err)
	}
	for index, part := range receipts {
		number := index + 1
		if part.PartNumber != uint32(number) || part.SizeBytes != uint64(number) || part.ETag != "fixture" || part.ChecksumSHA256 != listPartsDigest {
			t.Fatal("分页回执的顺序或原值错误", number)
		}
	}
}

func TestListUploadPartsRejectsIncompleteTraversal(t *testing.T) {
	first := minio.ObjectPart{PartNumber: 3, Size: 1, ETag: "first", ChecksumSHA256: listPartsDigest}
	for _, test := range []struct {
		name       string
		secondPage http.HandlerFunc
	}{
		{"empty after promised continuation", func(response http.ResponseWriter, _ *http.Request) {
			writeUploadPartsPageFixture(t, response, 3, 0, nil)
		}},
		{"repeated number across pages", func(response http.ResponseWriter, _ *http.Request) {
			writeUploadPartsPageFixture(t, response, 3, 0, []minio.ObjectPart{first})
		}},
		{"storage rejection on next page", func(response http.ResponseWriter, _ *http.Request) {
			response.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(response, "<Error><Code>AccessDenied</Code><Message>private-storage-response</Message></Error>")
		}},
		{"invalid next marker", func(response http.ResponseWriter, _ *http.Request) {
			part := minio.ObjectPart{PartNumber: 4, Size: 1, ETag: "second", ChecksumSHA256: listPartsDigest}
			writeUploadPartsPageFixture(t, response, 3, 3, []minio.ObjectPart{part})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			client := listPartsTestClient(t, func(response http.ResponseWriter, request *http.Request) {
				if calls.Add(1) == 1 {
					writeUploadPartsPageFixture(t, response, 0, 3, []minio.ObjectPart{first})
					return
				}
				test.secondPage(response, request)
			})
			receipts, err := client.ListUploadParts(t.Context(), listPartsFixtureKey, listPartsFixtureID, 1)
			if receipts != nil || err != domain.ErrUploadStorageUnavailable || calls.Load() != 2 {
				t.Fatal("续页失效后未丢弃整个结果", calls.Load(), err)
			}
		})
	}
}

func TestListUploadPartsCancelsDuringNextPage(t *testing.T) {
	var calls atomic.Int32
	secondPageStarted := make(chan struct{})
	client := listPartsTestClient(t, func(response http.ResponseWriter, request *http.Request) {
		switch calls.Add(1) {
		case 1:
			first := minio.ObjectPart{PartNumber: 1, Size: 1, ETag: "first", ChecksumSHA256: listPartsDigest}
			writeUploadPartsPageFixture(t, response, 0, 1, []minio.ObjectPart{first})
		case 2:
			close(secondPageStarted)
			select {
			case <-request.Context().Done():
			case <-time.After(2 * time.Second):
				t.Error("续页请求未及时取消")
				response.WriteHeader(http.StatusGatewayTimeout)
			}
		default:
			t.Error("取消后仍继续分页")
			response.WriteHeader(http.StatusBadRequest)
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		select {
		case <-secondPageStarted:
			cancel()
		case <-ctx.Done():
		}
	}()
	receipts, err := client.ListUploadParts(ctx, listPartsFixtureKey, listPartsFixtureID, 1)
	if receipts != nil || !errors.Is(err, context.Canceled) || calls.Load() != 2 {
		t.Fatal("取消续页没有丢弃已获取的第一页", calls.Load(), err)
	}
}
