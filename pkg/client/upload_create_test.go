package client

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCreateSingleUploadUsesStoredDeclaration(t *testing.T) {
	const key = "c4ce88e7-fd87-4753-837c-145cb682728d"
	declaration := SingleUploadDeclaration{ParentID: "18446744073709551614", Name: "空文件.txt", SHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}
	for _, sample := range []struct {
		name        string
		contentPath string
		wantError   bool
	}{
		{name: "matching response", contentPath: "/api/v1/uploads/19/content"},
		{name: "different task path", contentPath: "/api/v1/uploads/20/content", wantError: true},
	} {
		t.Run(sample.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodPost || request.URL.Path != "/api/v1/uploads" ||
					request.Header.Get("Idempotency-Key") != key || request.Header.Get("Authorization") != "Bearer sample" {
					t.Error("request did not use the stored creation key and current session")
				}
				var payload struct {
					ParentID  string `json:"parent_id"`
					Name      string `json:"name"`
					SizeBytes *int64 `json:"size_bytes"`
					SHA256    string `json:"sha256"`
				}
				if err := json.NewDecoder(request.Body).Decode(&payload); err != nil ||
					payload.ParentID != declaration.ParentID || payload.Name != declaration.Name ||
					payload.SizeBytes == nil || *payload.SizeBytes != 0 || payload.SHA256 != declaration.SHA256 {
					t.Error("request did not preserve the original declaration, including the explicit zero size")
				}
				writer.WriteHeader(http.StatusCreated)
				fmt.Fprintf(writer, `{"data":{"task":{"id":"19","parent_id":%q,"size_bytes":0,"status":"UPLOADING","upload_mode":"SINGLE",`+
					`"expires_at":"2026-11-25T12:00:00Z"},"upload_mode":"SINGLE","content_path":%q,"expires_at":"2026-11-25T12:00:00Z"}}`,
					declaration.ParentID, sample.contentPath)
			}))
			defer server.Close()
			receipt, err := CreateSingleUpload(t.Context(), server.URL, "sample", key, declaration)
			if sample.wantError {
				if err == nil || receipt.TaskID != "" {
					t.Fatal("unexpected content path was accepted")
				}
			} else if err != nil || receipt.TaskID != "19" || receipt.ContentPath != sample.contentPath || receipt.ExpiresAt.IsZero() {
				t.Fatalf("valid receipt was not returned: %+v, %v", receipt, err)
			}
		})
	}
}

func TestCreateSingleUploadRejectsMultipartSizeBeforeRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		t.Error("multipart-sized declaration reached the server")
		writer.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()
	if _, err := CreateSingleUpload(t.Context(), server.URL, "sample", "key", SingleUploadDeclaration{SizeBytes: MaxSingleUploadBytes + 1}); err == nil {
		t.Fatal("multipart-sized source was accepted by SINGLE creation")
	}
}
