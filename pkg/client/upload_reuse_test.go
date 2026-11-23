package client

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReuseUploadContentAcceptsOnlyScopedHitOrExplicitMiss(t *testing.T) {
	const digest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	declared := UploadDeclaration{ParentID: "20", Name: "empty.txt", SizeBytes: 0, SHA256: digest}
	const reply = `{"data":{"entry":{"id":"18446744073709551615","parent_id":"20","name":"empty (1).txt","entry_type":"FILE","size_bytes":0,"revision":"1","status":"ACTIVE"}}}`
	for _, fixture := range []struct {
		name, body string
		status     int
		wanted     error
	}{
		{"hit", reply, 201, nil},
		{"miss", `{"error":{"code":"REUSABLE_CONTENT_NOT_FOUND"}}`, 404, ErrReusableContentNotFound},
		{"expired access", `{"error":{"code":"TOKEN_EXPIRED"}}`, 401, ErrAccessTokenExpired},
		{"wrong directory", strings.Replace(reply, `"parent_id":"20"`, `"parent_id":"21"`, 1), 201, nil},
		{"missing size", strings.Replace(reply, `"size_bytes":0,`, "", 1), 201, nil},
		{"directory not found", `{"error":{"code":"ENTRY_NOT_FOUND"}}`, 404, nil},
		{"quota rejection", `{"error":{"code":"QUOTA_EXCEEDED"}}`, 507, nil},
		{"storage error with miss code", `{"error":{"code":"REUSABLE_CONTENT_NOT_FOUND"}}`, 503, nil},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				body, err := io.ReadAll(request.Body)
				want := fmt.Sprintf(`{"parent_id":"20","name":"empty.txt","size_bytes":0,"sha256":%q}`, digest)
				if err != nil || string(body) != want || request.Method != http.MethodPost ||
					request.URL.RequestURI() != "/api/v1/files/reuse" || request.Header.Get("Authorization") != "Bearer sample" ||
					request.Header.Get("Idempotency-Key") != "saved-key" {
					t.Error("request changed the saved declaration, account, or key")
				}
				writer.WriteHeader(fixture.status)
				_, _ = io.WriteString(writer, fixture.body)
			}))
			defer server.Close()
			receipt, err := ReuseUploadContent(t.Context(), server.URL, "sample", "saved-key", declared)
			if fixture.name == "hit" {
				if err != nil || receipt != (UploadReuseReceipt{EntryID: "18446744073709551615", Name: "empty (1).txt"}) {
					t.Fatalf("unexpected reuse receipt: %+v, %v", receipt, err)
				}
				return
			}
			if err == nil || receipt != (UploadReuseReceipt{}) ||
				errors.Is(err, ErrReusableContentNotFound) != (fixture.wanted == ErrReusableContentNotFound) ||
				errors.Is(err, ErrAccessTokenExpired) != (fixture.wanted == ErrAccessTokenExpired) {
				t.Fatalf("unexpected failure classification: %+v, %v", receipt, err)
			}
		})
	}
}
