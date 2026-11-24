package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFileEditReceipts(t *testing.T) {
	for _, scenario := range []struct {
		name, method, suffix, target, receiptParent, kind string
		wantError                                         bool
	}{
		{name: "rename detail", method: http.MethodPatch, receiptParent: "2", kind: "document"},
		{name: "move detail", method: http.MethodPost, suffix: "/move", target: "7", receiptParent: "7", kind: "document"},
		{name: "wrong destination", method: http.MethodPost, suffix: "/move", target: "7", receiptParent: "2", kind: "document", wantError: true},
		{name: "folder receipt", method: http.MethodPatch, receiptParent: "2", kind: "folder", wantError: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != scenario.method || request.URL.Path != "/api/v1/entries/3"+scenario.suffix ||
					request.Header.Get("Idempotency-Key") != "same-operation" || request.Header.Get("Authorization") != "Bearer token" {
					t.Errorf("unexpected edit request: %s %s", request.Method, request.URL.Path)
				}
				var body map[string]string
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Errorf("invalid request JSON: %v", err)
				}
				if body["expected_revision"] != "4" || body["target_parent_id"] != scenario.target ||
					(scenario.target == "" && body["name"] != "notes.txt") || len(body) != 2 {
					t.Errorf("unexpected request body: %v", body)
				}
				fmt.Fprintf(writer, `{"data":{"entry":{"id":"3","parent_id":%q,"name":"notes.txt","revision":"5","kind":%q,"status":"ACTIVE","size_bytes":23}}}`, scenario.receiptParent, scenario.kind)
			}))
			defer server.Close()

			var entry DirectoryEntry
			var err error
			if scenario.target == "" {
				entry, err = RenameFile(context.Background(), server.URL, "token", "3", "4", "2", "notes.txt", "same-operation")
			} else {
				entry, err = MoveFile(context.Background(), server.URL, "token", "3", "4", "2", scenario.target, "same-operation")
			}
			if scenario.wantError {
				if err == nil {
					t.Fatal("accepted mismatched receipt")
				}
				return
			}
			if err != nil || entry != (DirectoryEntry{ID: "3", ParentID: scenario.receiptParent, Name: "notes.txt", EntryType: "FILE", Revision: "5", SizeBytes: 23}) {
				t.Fatalf("unexpected result: %+v, %v", entry, err)
			}
		})
	}
}
