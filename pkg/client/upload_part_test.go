package client

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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
