package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/pkg/client"
)

func TestUploadReuseKeepsPathAcrossFailuresAndPersistsReceipt(t *testing.T) {
	for _, fixture := range []struct {
		name, status, body string
		code               int
	}{
		{"hit", "REUSED", `{"data":{"entry":{"id":"44","parent_id":"20","name":"copy (1).bin","entry_type":"FILE","size_bytes":1,"revision":"1","status":"ACTIVE"}}}`, 201},
		{"miss", "PENDING_CREATE", `{"error":{"code":"REUSABLE_CONTENT_NOT_FOUND"}}`, 404},
		{"unknown", "REUSE_PENDING", "", 503},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Header.Get("Idempotency-Key") != "saved-key" {
					t.Error("replay changed the stored creation key")
				}
				writer.WriteHeader(fixture.code)
				_, _ = writer.Write([]byte(fixture.body))
			}))
			defer server.Close()
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "transfers.sqlite3")
			db, err := openUploadIntents(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			owner := uploadOwner{Address: server.URL, UserID: "1"}
			digest := strings.Repeat("a", 64)
			if _, err := db.ExecContext(ctx, `INSERT INTO upload_intents
				(id, server_address, user_id, parent_id, source_path, name, size_bytes, sha256, creation_key, status)
				VALUES (1, ?, ?, '20', '', 'copy.bin', 1, ?, 'saved-key', 'REUSE_PENDING')`, owner.Address, owner.UserID, digest); err != nil {
				t.Fatal(err)
			}
			submission, err := readUploadSubmission(ctx, db, owner, 1)
			if err != nil {
				t.Fatal(err)
			}
			err = tryUploadReuse(ctx, scopedUploadIntent{db: db, owner: owner, id: 1, accessToken: "sample"}, submission)
			if (err == nil) != (fixture.name == "hit") ||
				errors.Is(err, client.ErrReusableContentNotFound) != (fixture.name == "miss") {
				t.Fatalf("unexpected result from reuse attempt: %v", err)
			}
			items, err := listUploadIntents(ctx, db, owner)
			if err != nil || len(items) != 1 || items[0].Status != fixture.status {
				t.Fatalf("wrong recovery path: %+v, %v", items, err)
			}
			if fixture.name == "hit" && (items[0].ResultEntryID != "44" || items[0].ResultName != "copy (1).bin" || items[0].TaskID != "") {
				t.Fatalf("reuse result did not retain the new entry and actual name: %+v", items[0])
			}
		})
	}
}
