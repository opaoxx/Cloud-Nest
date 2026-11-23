package client

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCancelUploadTaskChecksHTTPStatusAndTaskIdentity(t *testing.T) {
	for _, fixture := range []struct {
		name, mode, parent, status, entry, completedAt string
		httpStatus                                     int
		wantError                                      error
	}{
		{"single accepted", "SINGLE", "20", "CANCELLING", "null", "null", 202, nil},
		{"multipart cancelled", "MULTIPART", "20", "CANCELLED", "null", "null", 200, nil},
		{"publication won", "MULTIPART", "20", "COMPLETED", `"99"`, `"2026-11-24T03:00:00Z"`, 200, nil},
		{"wrong directory", "SINGLE", "21", "CANCELLED", "null", "null", 200, errUploadCancellationUnknown},
		{"terminal acceptance", "SINGLE", "20", "CANCELLED", "null", "null", 202, errUploadCancellationUnknown},
		{"unfinished success", "MULTIPART", "20", "CANCELLING", "null", "null", 200, errUploadCancellationUnknown},
		{"publication missing entry", "MULTIPART", "20", "COMPLETED", "null", `"2026-11-24T03:00:00Z"`, 200, errUploadCancellationUnknown},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			target := UploadTaskTarget{TaskID: "33", ParentID: "20", Mode: fixture.mode, SizeBytes: 3}
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				body, err := io.ReadAll(request.Body)
				if err != nil || len(body) != 0 || request.Method != http.MethodPost ||
					request.URL.RequestURI() != "/api/v1/uploads/33/cancel" ||
					request.Header.Get("Authorization") != "Bearer sample" || request.Header.Get("Idempotency-Key") != "" {
					t.Error("cancel request does not match the original task and no-body contract")
				}
				writer.WriteHeader(fixture.httpStatus)
				fmt.Fprintf(writer, `{"data":{"id":"33","parent_id":%q,"size_bytes":3,"upload_mode":%q,
					"status":%q,"result_entry_id":%s,"completed_at":%s}}`,
					fixture.parent, fixture.mode, fixture.status, fixture.entry, fixture.completedAt)
			}))
			defer server.Close()
			snapshot, err := CancelUploadTask(t.Context(), server.URL, "sample", target)
			if !errors.Is(err, fixture.wantError) {
				t.Fatalf("unexpected error: %v", err)
			}
			if err == nil && (snapshot.Status != fixture.status || (snapshot.Status == "COMPLETED" && snapshot.ResultEntryID != "99")) {
				t.Fatalf("unexpected cancellation snapshot: %+v", snapshot)
			}
		})
	}
}
