package client

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCompleteMultipartUploadValidatesOriginalTaskAndAcceptance(t *testing.T) {
	const order = `{"parts":[{"part_number":1,"etag":"first"},{"part_number":2,"etag":"last"}]}`
	plan := MultipartPlan{SizeBytes: MaxSingleUploadBytes + 1, PartSizeBytes: MaxSingleUploadBytes, PartCount: 2}
	receipts := []UploadPartReceipt{{Number: 1, SizeBytes: MaxSingleUploadBytes, ETag: "first"}, {Number: 2, SizeBytes: 1, ETag: "last"}}
	for _, fixture := range []struct {
		name, status, entry, completedAt string
		httpStatus                       int
		unknown                          bool
	}{
		{"accepted merge", "COMPLETING", "null", "null", 202, false},
		{"accepted verification", "VERIFYING", "null", "null", 202, false},
		{"published", "COMPLETED", `"99"`, `"2026-11-24T03:00:00Z"`, 200, false},
		{"unexpected acceptance", "UPLOADING", "null", "null", 202, true},
		{"missing entry", "COMPLETED", "null", `"2026-11-24T03:00:00Z"`, 200, true},
		{"missing publication time", "COMPLETED", `"99"`, "null", 200, true},
		{"unavailable", "", "", "", 503, true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				body, err := io.ReadAll(request.Body)
				if err != nil || string(body) != order || request.Method != http.MethodPost ||
					request.URL.RequestURI() != "/api/v1/uploads/33/complete" ||
					request.Header.Get("Authorization") != "Bearer sample" || request.Header.Get("Idempotency-Key") != "" {
					t.Error("completion request changed task identity or the ordered receipt list")
				}
				writer.WriteHeader(fixture.httpStatus)
				if fixture.status != "" {
					fmt.Fprintf(writer, `{"data":{"id":"33","parent_id":"20","size_bytes":16777217,"upload_mode":"MULTIPART",
						"status":%q,"result_entry_id":%s,"completed_at":%s}}`, fixture.status, fixture.entry, fixture.completedAt)
				}
			}))
			defer server.Close()
			snapshot, err := CompleteMultipartUpload(t.Context(), server.URL, "sample", "33", "20", plan, receipts)
			if fixture.unknown {
				if !errors.Is(err, ErrMultipartCompletionUnknown) || snapshot != (UploadTaskSnapshot{}) {
					t.Fatalf("expected an unknown result, got %+v, %v", snapshot, err)
				}
			} else if err != nil || snapshot.Status != fixture.status || (snapshot.Status == "COMPLETED" && snapshot.ResultEntryID != "99") {
				t.Fatalf("unexpected completion snapshot: %+v, %v", snapshot, err)
			}
		})
	}
}

func TestCompleteMultipartUploadRejectsInvalidLocalReceiptsBeforePOST(t *testing.T) {
	plan := MultipartPlan{SizeBytes: MaxSingleUploadBytes + 1, PartSizeBytes: MaxSingleUploadBytes, PartCount: 2}
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("invalid receipts reached the remote task")
	}))
	defer server.Close()

	first := UploadPartReceipt{Number: 1, SizeBytes: MaxSingleUploadBytes, ETag: "first"}
	last := UploadPartReceipt{Number: 2, SizeBytes: 1, ETag: "last"}
	for _, receipts := range [][]UploadPartReceipt{
		{first}, {last, first}, {first, first}, {first, {Number: 2, SizeBytes: 2, ETag: "last"}},
	} {
		if _, err := CompleteMultipartUpload(t.Context(), server.URL, "sample", "33", "20", plan, receipts); err == nil {
			t.Fatalf("invalid receipts were accepted: %+v", receipts)
		}
	}
}
