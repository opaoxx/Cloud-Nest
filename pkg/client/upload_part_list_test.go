package client

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReadUploadPartsChecksPlanAndNormalizesChecksum(t *testing.T) {
	digest := sha256.Sum256([]byte("x"))
	part := fmt.Sprintf(`{"part_number":2,"size_bytes":1,"etag":"part-tag","checksum_sha256":%q}`, base64.StdEncoding.EncodeToString(digest[:]))
	plan := MultipartPlan{SizeBytes: MaxSingleUploadBytes + 1, PartSizeBytes: MaxSingleUploadBytes, PartCount: 2}
	for _, fixture := range []struct {
		name, parts         string
		partCount, wantSize int
		wantError           bool
	}{
		{"matching tail", "[" + part + "]", 2, 1, false},
		{"empty list", "[]", 2, 0, false},
		{"null list", "null", 2, 0, true},
		{"wrong plan", "[]", 3, 0, true},
		{"duplicate", "[" + part + "," + part + "]", 2, 0, true},
		{"invalid checksum", `[{"part_number":2,"size_bytes":1,"etag":"part-tag","checksum_sha256":"invalid"}]`, 2, 0, true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodGet || request.URL.RequestURI() != "/api/v1/uploads/33/parts" ||
					request.Header.Get("Authorization") != "Bearer sample" {
					t.Error("storage receipt query did not use the saved task and current session")
				}
				fmt.Fprintf(writer, `{"data":{"part_size_bytes":16777216,"part_count":%d,"parts":%s}}`, fixture.partCount, fixture.parts)
			}))
			defer server.Close()
			receipts, err := ReadUploadParts(t.Context(), server.URL, "sample", "33", plan)
			if (err != nil) != fixture.wantError || len(receipts) != fixture.wantSize {
				t.Fatalf("unexpected reply: %+v, %v", receipts, err)
			}
			if fixture.wantSize == 1 && receipts[0] != (UploadPartReceipt{Number: 2, SizeBytes: 1, SHA256: hex.EncodeToString(digest[:]), ETag: "part-tag"}) {
				t.Fatalf("wrong storage receipt: %+v", receipts[0])
			}
		})
	}
}
