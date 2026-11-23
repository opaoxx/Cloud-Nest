package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestRestoreWindowsSessionRotatesProtectedCredential(t *testing.T) {
	var refreshes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.Header.Get("X-CloudNest-Client") != "native" ||
			request.Header.Get("Origin") != "" || request.Header.Get("Cookie") != "" {
			t.Error("request does not match the native authentication contract")
		}
		switch request.URL.Path {
		case "/api/v1/auth/login":
			fmt.Fprint(writer, `{"data":{"user":{"username":"root"},"access_token":"initial-access","refresh_token":"initial-refresh"}}`)
		case "/api/v1/auth/refresh":
			var payload map[string]string
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil || payload["client_type"] != "native" {
				t.Error("invalid native refresh payload")
			}
			if refreshes.Add(1) == 1 {
				if payload["refresh_token"] != "initial-refresh" {
					t.Error("first restore did not send the stored credential")
				}
				fmt.Fprint(writer, `{"data":{"access_token":"next-access","refresh_token":"next-refresh"}}`)
				return
			}
			if payload["refresh_token"] != "next-refresh" {
				t.Error("second restore did not send the rotated credential")
			}
			writer.WriteHeader(http.StatusUnauthorized)
		default:
			t.Error("unexpected authentication path")
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "session.bin")
	installationID := "c4ce88e7-fd87-4753-837c-145cb682728d"
	file := sessionFile{path: path, installationID: installationID}
	first := &Account{installationID: installationID, credentials: file}
	if _, err := first.Login(t.Context(), server.URL, "root", "sample-password"); err != nil {
		t.Fatal(err)
	}
	assertProtected := func() {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"initial-access", "initial-refresh", "next-access", "next-refresh", "sample-password"} {
			if bytes.Contains(data, []byte(secret)) || bytes.Contains(data, testUTF16ASCII(secret)) {
				t.Fatal("plaintext secret found in saved session")
			}
		}
	}
	assertProtected()
	otherInstallation := sessionFile{path: path, installationID: "07fde863-e512-4bb0-9987-3d5a0c71db04"}
	if _, err := otherInstallation.read(); err == nil {
		t.Fatal("credential opened with a different installation ID")
	}

	second := &Account{installationID: installationID, credentials: file}
	view, err := second.Restore(t.Context())
	if err != nil || view.Username != "root" || view.Address != server.URL || second.session.AccessToken != "next-access" {
		t.Fatalf("restore did not create an in-memory session: %v", err)
	}
	assertProtected()
	third := &Account{installationID: installationID, credentials: file}
	view, err = third.Restore(t.Context())
	if err != nil || view.Username != "" || view.Notice == "" {
		t.Fatalf("rejected credentials did not return to login: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stale saved credential still present: %v", err)
	}
}

func TestSessionFileLockRejectsConcurrentAccess(t *testing.T) {
	file := sessionFile{path: filepath.Join(t.TempDir(), "session.bin")}
	first, err := file.lock()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := file.lock(); err == nil {
		second.Close()
		t.Fatal("second handle locked the same session file")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := file.lock()
	if err != nil {
		t.Fatal(err)
	}
	third.Close()
}

func testUTF16ASCII(text string) []byte {
	var encoded []byte
	for _, character := range []byte(text) {
		encoded = append(encoded, character, 0)
	}
	return encoded
}
