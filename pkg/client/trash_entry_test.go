package client

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTrashFileChecksRevisionAndResult(t *testing.T) {
	for _, scenario := range []struct {
		name, reply, wantError string
		status                 int
	}{
		{"confirmed", `{"data":{"entry":{"id":"9","entry_type":"FILE","revision":"8","status":"TRASHED","deleted_at":"2026-11-24T00:00:00Z"}}}`, "", 200},
		{"wrong file", `{"data":{"entry":{"id":"10","entry_type":"FILE","revision":"8","status":"TRASHED","deleted_at":"2026-11-24T00:00:00Z"}}}`, "回收结果有误", 200},
		{"stale revision", `{"error":{"code":"REVISION_CONFLICT"}}`, "文件已发生变化", 409},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil || r.Method != http.MethodPost || r.URL.Path != "/api/v1/entries/9/trash" ||
					r.Header.Get("Idempotency-Key") != "trash-attempt" ||
					r.Header.Get("Authorization") != "Bearer fixture" ||
					string(body) != `{"expected_revision":"7"}` {
					t.Error("unexpected trash request")
				}
				w.WriteHeader(scenario.status)
				_, _ = io.WriteString(w, scenario.reply)
			}))
			defer server.Close()
			err := TrashFile(context.Background(), server.URL, "fixture", "9", "7", "trash-attempt")
			if scenario.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), scenario.wantError) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
