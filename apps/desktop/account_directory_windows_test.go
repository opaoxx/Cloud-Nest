package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestReadRootDirectoryRefreshesExpiredDirectoryRequest(t *testing.T) {
	const rootID = "18446744073709551614"
	requests := make(chan string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		token := request.Header.Get("Authorization")
		requests <- request.URL.RequestURI() + " " + token
		switch request.URL.Path {
		case "/api/v1/auth/login":
			fmt.Fprint(writer, `{"data":{"user":{"username":"root"},"access_token":"old-access","refresh_token":"old-refresh"}}`)
		case "/api/v1/me":
			fmt.Fprintf(writer, `{"data":{"user":{"username":"root"},"root_entry_id":%q}}`, rootID)
		case "/api/v1/entries":
			if request.URL.Query().Get("parent_id") != rootID {
				t.Error("directory request changed the root ID")
			}
			if token == "Bearer old-access" {
				writer.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(writer, `{"error":{"code":"TOKEN_EXPIRED"}}`)
				return
			}
			fmt.Fprintf(writer, `{"data":{"items":[],"breadcrumbs":[{"id":%q,"name":""}],"next_cursor":null}}`, rootID)
		case "/api/v1/auth/refresh":
			fmt.Fprint(writer, `{"data":{"access_token":"new-access","refresh_token":"new-refresh"}}`)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	file := sessionFile{path: filepath.Join(t.TempDir(), "session.bin"), installationID: "c4ce88e7-fd87-4753-837c-145cb682728d"}
	account := &Account{installationID: file.installationID, credentials: file}
	if _, err := account.Login(t.Context(), server.URL, "root", "password"); err != nil {
		t.Fatal(err)
	}
	result, err := account.ReadRootDirectory(t.Context())
	if err != nil || result.LoginRequired != "" || result.Page.Items == nil || len(result.Page.Items) != 0 {
		t.Fatalf("root request did not return an empty page: %+v, %v", result, err)
	}
	close(requests)
	var received []string
	for request := range requests {
		received = append(received, request)
	}
	expected := []string{
		"/api/v1/auth/login ",
		"/api/v1/me Bearer old-access",
		"/api/v1/entries?parent_id=" + rootID + " Bearer old-access",
		"/api/v1/auth/refresh ",
		"/api/v1/me Bearer new-access",
		"/api/v1/entries?parent_id=" + rootID + " Bearer new-access",
	}
	if len(received) != len(expected) {
		t.Fatalf("unexpected number of requests: %v", received)
	}
	for index, want := range expected {
		if received[index] != want {
			t.Errorf("request %d = %q, want %q", index, received[index], want)
		}
	}
}
