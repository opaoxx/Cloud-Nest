package storage

import (
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestCheckMultipartUploadSessionExists(t *testing.T) {
	for _, fixture := range []struct {
		name, code, requestID string
		status                int
		exists                bool
		wantErr               error
	}{
		{name: "empty active session", status: http.StatusOK, exists: true},
		{name: "explicitly missing", status: http.StatusNotFound, code: "NoSuchUpload", requestID: "fixture-request"},
		{name: "missing response identity", status: http.StatusNotFound, code: "NoSuchUpload", wantErr: domain.ErrUploadStorageUnavailable},
		{name: "foreign error code", status: http.StatusNotFound, code: "NoSuchBucket", requestID: "fixture-request", wantErr: domain.ErrUploadStorageUnavailable},
		{name: "unexpected status", status: http.StatusForbidden, code: "NoSuchUpload", requestID: "fixture-request", wantErr: domain.ErrUploadStorageUnavailable},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			var calls atomic.Int32
			client := listPartsTestClient(t, func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				query := request.URL.Query()
				if query.Get("uploadId") != listPartsFixtureID || query.Get("part-number-marker") != "0" || query.Get("max-parts") != "1" {
					t.Error("未按原会话读取第一页的一条分片")
				}
				if fixture.status == http.StatusOK {
					writeUploadPartsPageFixture(t, writer, 0, 0, nil)
					return
				}
				writer.Header().Set("x-amz-request-id", fixture.requestID)
				writer.WriteHeader(fixture.status)
				_, _ = io.WriteString(writer, "<Error><Code>"+fixture.code+"</Code><Message>fixture</Message></Error>")
			})
			exists, err := client.CheckMultipartUploadSessionExists(t.Context(), listPartsFixtureKey, listPartsFixtureID)
			if exists != fixture.exists || !errors.Is(err, fixture.wantErr) || calls.Load() != 1 {
				t.Fatal("原会话存在性、错误或请求数不符", exists, err, calls.Load())
			}
		})
	}
}
