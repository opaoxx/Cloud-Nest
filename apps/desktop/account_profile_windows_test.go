package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestConcurrentProfileReadsShareRefresh(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		dropReply bool
	}{
		{name: "rotation and retry"},
		{name: "lost rotation reply", dropReply: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var expiredReads, newReads, refreshes atomic.Int32
			bothExpired := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/api/v1/auth/login":
					fmt.Fprint(writer, `{"data":{"user":{"username":"root"},"access_token":"old-access","refresh_token":"old-refresh"}}`)
				case "/api/v1/me":
					switch request.Header.Get("Authorization") {
					case "Bearer old-access":
						// 两个请求都必须先把旧令牌发出去，才向它们返回过期响应。
						if expiredReads.Add(1) == 2 {
							close(bothExpired)
						}
						select {
						case <-bothExpired:
						case <-request.Context().Done():
							return
						}
						writer.WriteHeader(http.StatusUnauthorized)
						fmt.Fprint(writer, `{"error":{"code":"TOKEN_EXPIRED"}}`)
					case "Bearer new-access":
						newReads.Add(1)
						fmt.Fprint(writer, `{"data":{"user":{"username":"root"}}}`)
					default:
						t.Error("profile request used an unexpected token")
						writer.WriteHeader(http.StatusUnauthorized)
					}
				case "/api/v1/auth/refresh":
					var payload map[string]string
					if err := json.NewDecoder(request.Body).Decode(&payload); err != nil || payload["refresh_token"] != "old-refresh" {
						t.Error("refresh did not use the original credential")
					}
					refreshes.Add(1)
					if scenario.dropReply {
						// 连接断开时客户端收不到响应，无法判断服务端是否已完成轮换。
						connection, _, err := writer.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						connection.Close()
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
			type readResult struct {
				profile ProfileReadResult
				err     error
			}
			results := make(chan readResult, 2)
			for range 2 {
				go func() {
					profile, err := account.ReadProfile(t.Context())
					results <- readResult{profile: profile, err: err}
				}()
			}
			for range 2 {
				result := <-results
				if result.err != nil {
					t.Fatal(result.err)
				}
				if scenario.dropReply && (result.profile.LoginRequired == "" || result.profile.Username != "") {
					t.Fatalf("read did not return to login: %+v", result.profile)
				}
				if !scenario.dropReply && result.profile.Username != "root" {
					t.Fatalf("profile retry did not succeed: %+v", result.profile)
				}
			}
			if expiredReads.Load() != 2 || refreshes.Load() != 1 {
				t.Fatalf("expired reads=%d, refreshes=%d", expiredReads.Load(), refreshes.Load())
			}
			if scenario.dropReply {
				if _, err := os.Stat(file.path); !errors.Is(err, os.ErrNotExist) || newReads.Load() != 0 {
					t.Fatal("lost refresh reply kept stale credentials or retried the read")
				}
				if _, err := account.Login(t.Context(), server.URL, "root", "password"); err != nil {
					t.Fatalf("manual login failed after the refresh was lost: %v", err)
				}
			} else {
				record, err := file.read()
				if err != nil || record.RefreshToken != "new-refresh" || newReads.Load() != 2 {
					t.Fatal("rotated credential was not persisted or both reads were not retried")
				}
			}
		})
	}
}
