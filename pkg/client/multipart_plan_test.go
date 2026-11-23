package client

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCreateUploadValidatesMultipartReceipt(t *testing.T) {
	declaration := UploadDeclaration{ParentID: "20", Name: "分片.bin", SizeBytes: MaxSingleUploadBytes + 1, SHA256: strings.Repeat("a", 64)}
	for _, fixture := range []struct {
		name, status, mode, plan string
		wantError                bool
	}{
		{"matching", "CREATING", "MULTIPART", `"part_size_bytes":16777216,"part_count":2`, false},
		{"wrong count", "CREATING", "MULTIPART", `"part_size_bytes":16777216,"part_count":1`, true},
		{"unaligned size", "CREATING", "MULTIPART", `"part_size_bytes":16777217,"part_count":1`, true},
		{"missing plan", "CREATING", "MULTIPART", `"part_size_bytes":null,"part_count":null`, true},
		{"wrong status", "UPLOADING", "MULTIPART", `"part_size_bytes":16777216,"part_count":2`, true},
		{"wrong mode", "CREATING", "SINGLE", `"part_size_bytes":16777216,"part_count":2`, true},
		{"wrong content path", "CREATING", "MULTIPART", `"part_size_bytes":16777216,"part_count":2,"content_path":"/api/v1/uploads/19/content"`, true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				var received UploadDeclaration
				if err := json.NewDecoder(request.Body).Decode(&received); err != nil || received != declaration ||
					request.Method != http.MethodPost || request.URL.Path != "/api/v1/uploads" ||
					request.Header.Get("Idempotency-Key") != "same-key" || request.Header.Get("Authorization") != "Bearer sample" {
					t.Error("unexpected multipart creation request")
				}
				writer.WriteHeader(http.StatusCreated)
				fmt.Fprintf(writer, `{"data":{"task":{"id":"19","parent_id":"20","size_bytes":16777217,"status":%q,"upload_mode":"MULTIPART",
					"expires_at":"2026-11-25T12:00:00Z"},"upload_mode":%q,"expires_at":"2026-11-25T12:00:00Z",%s}}`,
					fixture.status, fixture.mode, fixture.plan)
			}))
			defer server.Close()
			receipt, err := CreateUpload(t.Context(), server.URL, "sample", "same-key", declaration)
			if fixture.wantError {
				if err == nil || receipt != (UploadCreation{}) {
					t.Fatal("invalid multipart plan was accepted")
				}
			} else if err != nil || receipt.TaskID != "19" || receipt.ContentPath != "" || receipt.Multipart == nil ||
				*receipt.Multipart != (MultipartPlan{SizeBytes: MaxSingleUploadBytes + 1, PartSizeBytes: MaxSingleUploadBytes, PartCount: 2}) {
				t.Fatalf("valid multipart receipt lost its plan: %+v, %v", receipt, err)
			}
		})
	}
}

func TestMultipartPlanPartRange(t *testing.T) {
	for _, fixture := range []struct {
		size, wantOffset, wantSize int64
		count, part                int
		wantError                  bool
	}{
		{MaxSingleUploadBytes + 1, 0, MaxSingleUploadBytes, 2, 1, false},
		{MaxSingleUploadBytes + 1, MaxSingleUploadBytes, 1, 2, 2, false},
		{2 * MaxSingleUploadBytes, MaxSingleUploadBytes, MaxSingleUploadBytes, 2, 2, false},
		{MaxSingleUploadBytes + 1, 0, 0, 2, 0, true},
		{MaxSingleUploadBytes + 1, 0, 0, 2, 3, true},
		{10_001 * MaxSingleUploadBytes, 0, 0, 10_001, 1, true},
	} {
		plan := MultipartPlan{SizeBytes: fixture.size, PartSizeBytes: MaxSingleUploadBytes, PartCount: fixture.count}
		part, err := plan.PartRange(fixture.part)
		if (err != nil) != fixture.wantError || part.OffsetBytes != fixture.wantOffset || part.SizeBytes != fixture.wantSize {
			t.Fatalf("size %d, part %d: got %+v, %v", fixture.size, fixture.part, part, err)
		}
	}
}
