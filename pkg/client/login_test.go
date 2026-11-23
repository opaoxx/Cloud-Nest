package client

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLoginWindowsNativeContract(t *testing.T) {
	const installationID = "c4ce88e7-fd87-4753-837c-145cb682728d"
	const password = " test password "
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.RequestURI() != "/api/v1/auth/login" ||
			request.Header.Get("X-CloudNest-Client") != "native" || request.Header.Get("Content-Type") != "application/json" ||
			request.Header.Get("Origin") != "" || request.Header.Get("Cookie") != "" {
			t.Error("request does not match the native login contract")
		}
		var body map[string]string
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["password"] != password {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		if body["username"] != "root" || body["installation_id"] != installationID ||
			body["client_type"] != "native" || body["platform"] != "WINDOWS" || body["device_name"] == "" {
			t.Error("unexpected native login payload")
		}
		fmt.Fprint(writer, `{"data":{"user":{"username":"root"},"access_token":"access","refresh_token":"refresh"}}`)
	}))
	defer server.Close()

	session, err := LoginWindows(t.Context(), server.URL, installationID, "root", strings.TrimSpace(password))
	if err == nil || session != (NativeSession{}) {
		t.Fatal("incorrect password created a local session")
	}
	session, err = LoginWindows(t.Context(), server.URL, installationID, "root", password)
	if err != nil || session != (NativeSession{Username: "root", AccessToken: "access", RefreshToken: "refresh"}) {
		t.Fatalf("valid native response did not create a session: %v", err)
	}
}

func TestLoginWindowsRejectsWebResponseAndRedirect(t *testing.T) {
	for _, redirect := range []bool{false, true} {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.URL.Path != "/api/v1/auth/login" {
				t.Error("login followed a redirect")
			}
			if redirect {
				http.Redirect(writer, request, "/redirected", http.StatusTemporaryRedirect)
				return
			}
			fmt.Fprint(writer, `{"data":{"user":{"username":"root"},"access_token":"access"}}`)
		}))
		session, err := LoginWindows(t.Context(), server.URL, "installation", "root", "password")
		server.Close()
		if err == nil || session != (NativeSession{}) {
			t.Fatalf("unexpected response created a session, redirect=%t", redirect)
		}
	}
}
