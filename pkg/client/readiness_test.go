package client

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReadinessMatchesStatusAndBody(t *testing.T) {
	for _, sample := range []struct {
		name      string
		code      int
		body      string
		wantReady bool
	}{
		{"ready", 200, `{"status":"ready"}`, true},
		{"dependency unavailable", 503, `{"status":"not_ready"}`, false},
		{"wrong status", 503, `{"status":"ready"}`, false},
		{"live endpoint", 200, `{"status":"ok"}`, false},
		{"proxy page", 200, "<html>OK</html>", false},
		{"extra JSON", 200, `{"status":"ready"}{}`, false},
		{"oversized", 200, fmt.Sprintf("%4097s", `{"status":"ready"}`), false},
	} {
		t.Run(sample.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.RequestURI() != "/health/ready" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
				}
				w.WriteHeader(sample.code)
				fmt.Fprint(w, sample.body)
			}))
			defer server.Close()
			err := (&Readiness{}).Check(t.Context(), server.URL+"/")
			if (err == nil) != sample.wantReady {
				t.Fatalf("Check returned %v, want ready=%t", err, sample.wantReady)
			}
		})
	}
}

func TestReadinessRefusesRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health/ready" {
			t.Error("followed redirect")
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	}))
	defer server.Close()
	if (&Readiness{}).Check(t.Context(), server.URL) == nil {
		t.Fatal("redirect reported as ready")
	}
}
