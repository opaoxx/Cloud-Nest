package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestLogoutWithExpiredAccessToken(t *testing.T) {
	for _, refreshFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("refresh fails %t", refreshFails), func(t *testing.T) {
			requests := make(chan string, 4)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				var payload map[string]string
				if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				switch request.URL.Path {
				case "/api/v1/auth/login":
					fmt.Fprint(writer, `{"data":{"user":{"username":"root"},"access_token":"old-access","refresh_token":"old-refresh"}}`)
				case "/api/v1/auth/logout":
					if request.Method != http.MethodPost || request.Header.Get("X-CloudNest-Client") != "native" ||
						request.Header.Get("Origin") != "" || request.Header.Get("Cookie") != "" ||
						payload["client_type"] != "native" || len(payload) != 1 {
						t.Error("logout request does not match the native contract")
					}
					token := request.Header.Get("Authorization")
					requests <- token
					if token == "Bearer old-access" {
						writer.WriteHeader(http.StatusUnauthorized)
						fmt.Fprint(writer, `{"error":{"code":"TOKEN_EXPIRED"}}`)
						return
					}
					fmt.Fprint(writer, `{"data":{"revoked":true}}`)
				case "/api/v1/auth/refresh":
					requests <- "refresh"
					if payload["refresh_token"] != "old-refresh" {
						t.Error("unexpected refresh credential")
					}
					if refreshFails {
						writer.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					fmt.Fprint(writer, `{"data":{"access_token":"new-access","refresh_token":"new-refresh"}}`)
				default:
					t.Error("unexpected request")
					writer.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			file := sessionFile{path: filepath.Join(t.TempDir(), "session.bin"), installationID: "c4ce88e7-fd87-4753-837c-145cb682728d"}
			account := &Account{installationID: file.installationID, credentials: file}
			if _, err := account.Login(t.Context(), server.URL, "root", "password"); err != nil {
				t.Fatal(err)
			}
			notice, err := account.Logout(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			close(requests)
			if first, second := <-requests, <-requests; first != "Bearer old-access" || second != "refresh" {
				t.Fatalf("unexpected logout sequence: %q, %q", first, second)
			}
			if third := <-requests; refreshFails && third != "" || !refreshFails && third != "Bearer new-access" {
				t.Fatalf("unexpected final request: %q", third)
			}
			expectedNotice := "已退出账号，服务端会话已结束。"
			if refreshFails {
				expectedNotice = "已退出本机，未能确认服务端会话已撤销。"
			}
			if notice != expectedNotice || account.session.AccessToken != "" {
				t.Fatalf("unexpected local logout result: %q", notice)
			}
			if _, err := os.Stat(file.path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("credential file still present: %v", err)
			}
		})
	}
}

func TestDelayedProfileFromOldAccountAfterLogout(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	releaseRead := sync.OnceFunc(func() { close(release) })
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/auth/login":
			var payload map[string]string
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Error(err)
				return
			}
			name := payload["username"]
			fmt.Fprintf(writer, `{"data":{"user":{"username":%q},"access_token":%q,"refresh_token":%q}}`, name, name+"-access", name+"-refresh")
		case "/api/v1/auth/logout":
			fmt.Fprint(writer, `{"data":{"revoked":true}}`)
		case "/api/v1/me":
			if request.Header.Get("Authorization") == "Bearer root-access" {
				close(started)
				select {
				case <-release:
				case <-request.Context().Done():
					return
				}
				fmt.Fprint(writer, `{"data":{"user":{"username":"root"}}}`)
				return
			}
			fmt.Fprint(writer, `{"data":{"user":{"username":"guest"}}}`)
		}
	}))
	defer server.Close()
	defer releaseRead()
	file := sessionFile{path: filepath.Join(t.TempDir(), "session.bin"), installationID: "c4ce88e7-fd87-4753-837c-145cb682728d"}
	account := &Account{installationID: file.installationID, credentials: file}
	if _, err := account.Login(t.Context(), server.URL, "root", "password"); err != nil {
		t.Fatal(err)
	}
	stale := make(chan error, 1)
	go func() {
		result, err := account.ReadProfile(t.Context())
		if result != (ProfileReadResult{}) {
			err = fmt.Errorf("stale read returned account state: %+v", result)
		}
		stale <- err
	}()
	select {
	case <-started:
	case err := <-stale:
		t.Fatalf("old read failed to start: %v", err)
	}
	if _, err := account.Logout(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := account.Login(t.Context(), server.URL, "guest", "password"); err != nil {
		t.Fatal(err)
	}
	releaseRead()
	if err := <-stale; !errors.Is(err, errSessionChanged) {
		t.Fatalf("stale read should be discarded: %v", err)
	}
	if result, err := account.ReadProfile(t.Context()); err != nil || result.Username != "guest" {
		t.Fatalf("new session did not return its own profile: %+v, %v", result, err)
	}
}
