package client

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestUploadPartFollowsSignedHeadersAndValidatesETag(t *testing.T) {
	content := []byte{0, 2, 255, 10}
	digest := sha256.Sum256(content)
	const path = "/object?partNumber=1&uploadId=session%2B1&signature=%2Fexample"
	for _, fixture := range []struct{ name, checksum, tag string }{
		{"valid", base64.StdEncoding.EncodeToString(digest[:]), "\"part-etag\""},
		{"invalid checksum", "different-checksum", "\"part-etag\""},
		{"missing etag", base64.StdEncoding.EncodeToString(digest[:]), ""},
		{"malformed etag", base64.StdEncoding.EncodeToString(digest[:]), "\"bad tag\""},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			storage := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if fixture.name == "invalid checksum" {
					t.Error("invalid grant reached storage")
				}
				body, err := io.ReadAll(request.Body)
				if err != nil || !bytes.Equal(body, content) || request.Method != http.MethodPut ||
					request.URL.RequestURI() != path || request.ContentLength != int64(len(content)) ||
					len(request.TransferEncoding) != 0 || request.Header.Get(uploadPartChecksumHeader) != fixture.checksum ||
					request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" {
					t.Error("object request did not match signed length, checksum and raw body")
				}
				if fixture.tag != "" {
					writer.Header().Set("ETag", fixture.tag)
				}
			}))
			defer storage.Close()
			api := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				body, err := io.ReadAll(request.Body)
				if err != nil || string(body) != `{"parts":[{"part_number":1,"sha256":"`+hex.EncodeToString(digest[:])+`"}]}` ||
					request.Method != http.MethodPost || request.URL.Path != "/api/v1/uploads/33/part-urls" ||
					request.Header.Get("Authorization") != "Bearer sample" {
					t.Error("business request did not match the part declaration")
				}
				_ = json.NewEncoder(writer).Encode(map[string]any{"data": map[string]any{
					"expires_at": time.Now().Add(time.Hour),
					"parts": []any{map[string]any{"part_number": 1, "size_bytes": len(content), "url": storage.URL + path,
						"required_headers": map[string]string{uploadPartChecksumHeader: fixture.checksum},
						"url_expires_at":   time.Now().Add(time.Minute)}},
				}})
			}))
			defer api.Close()
			grant, err := IssueUploadPartURL(t.Context(), api.URL, "sample", "33", 1, int64(len(content)), digest)
			if fixture.name == "invalid checksum" {
				if err == nil {
					t.Fatal("wrong signed checksum was accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := SendUploadPart(t.Context(), grant, bytes.NewReader(content))
			if fixture.name != "valid" {
				if err == nil || receipt != (UploadPartReceipt{}) {
					t.Fatal("invalid response was treated as confirmed")
				}
			} else if err != nil || receipt != (UploadPartReceipt{Number: 1, SizeBytes: int64(len(content)), SHA256: hex.EncodeToString(digest[:]), ETag: "part-etag"}) {
				t.Fatalf("valid receipt not returned: %+v, %v", receipt, err)
			}
		})
	}
}

func TestUploadPartBatchRequiresUniqueNumbersAndMatchingOrder(t *testing.T) {
	digest := sha256.Sum256([]byte("part"))
	parts := []UploadPartDeclaration{{Number: 2, SizeBytes: 4, Digest: digest}, {Number: 3, SizeBytes: 4, Digest: digest}}
	for _, reorder := range []bool{false, true} {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			var payload struct {
				Parts []struct {
					Number int    `json:"part_number"`
					SHA256 string `json:"sha256"`
				} `json:"parts"`
			}
			digestHex := hex.EncodeToString(digest[:])
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil || len(payload.Parts) != 2 ||
				payload.Parts[0].Number != 2 || payload.Parts[1].Number != 3 ||
				payload.Parts[0].SHA256 != digestHex || payload.Parts[1].SHA256 != digestHex {
				t.Error("batch did not contain exactly the requested part declarations")
			}
			numbers := []int{2, 3}
			if reorder {
				numbers[0], numbers[1] = numbers[1], numbers[0]
			}
			var grants []map[string]any
			for _, number := range numbers {
				grants = append(grants, map[string]any{
					"part_number": number, "size_bytes": 4,
					"url":              fmt.Sprintf("http://storage.invalid/object?partNumber=%d&uploadId=session", number),
					"required_headers": map[string]string{uploadPartChecksumHeader: base64.StdEncoding.EncodeToString(digest[:])},
					"url_expires_at":   time.Now().Add(time.Minute),
				})
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"data": map[string]any{"parts": grants, "expires_at": time.Now().Add(time.Hour)}})
		}))
		grants, err := IssueUploadPartURLs(t.Context(), server.URL, "sample", "33", parts)
		server.Close()
		if reorder {
			if err == nil || len(grants) > 0 {
				t.Fatal("reordered grants were returned to the sender")
			}
		} else if err != nil || len(grants) != 2 || grants[0].number != 2 || grants[1].number != 3 {
			t.Fatalf("expected both grants, got %v", err)
		}
	}

	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("duplicate part number reached the server")
	}))
	defer server.Close()
	if _, err := IssueUploadPartURLs(t.Context(), server.URL, "sample", "33", []UploadPartDeclaration{parts[0], parts[0]}); err == nil {
		t.Fatal("duplicate part number was accepted")
	}
}
