package client

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDownloadContentChecksSizeAndDigest(t *testing.T) {
	for _, fixture := range []struct {
		name, expectedBody, actualBody, contentLength, wantError string
		chunked                                                  bool
	}{
		{name: "binary", expectedBody: "\x00\xff\n", actualBody: "\x00\xff\n", contentLength: "3"},
		{name: "empty", contentLength: "0"},
		{name: "correct chunked body", expectedBody: "abc", actualBody: "abc", chunked: true},
		{name: "truncated connection", expectedBody: "abc", actualBody: "ab", contentLength: "3", wantError: "下载正文被截断，请重试"},
		{name: "short chunked body", expectedBody: "abc", actualBody: "ab", chunked: true, wantError: "下载正文被截断，请重试"},
		{name: "wrong declared length", expectedBody: "abc", actualBody: "ab", contentLength: "2", wantError: "下载正文大小与授权不符，请重试"},
		{name: "extra chunked byte", expectedBody: "abc", actualBody: "abcd", chunked: true, wantError: "下载正文超过授权大小，请重试"},
		{name: "same length corrupted body", expectedBody: "abc", actualBody: "abd", contentLength: "3", wantError: "下载正文 SHA-256 校验失败，请重试"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			const signedPath = "/bucket/a%2Fb?part=a%2Bb&sig=%2Ftest"
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodGet || request.URL.RequestURI() != signedPath ||
					request.Header.Get("Authorization") != "" || request.Header.Get("Range") != "" ||
					request.Header.Get("Accept-Encoding") != "identity" {
					t.Error("object request changed the signed URL or sent unexpected headers")
				}
				if fixture.chunked {
					writer.(http.Flusher).Flush()
				} else {
					writer.Header().Set("Content-Length", fixture.contentLength)
				}
				fmt.Fprint(writer, fixture.actualBody)
			}))
			defer server.Close()

			grant := DownloadGrant{URL: server.URL + signedPath, ExpiresAt: time.Now().Add(time.Minute),
				SizeBytes: uint64(len(fixture.expectedBody)), SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(fixture.expectedBody)))}
			var destination bytes.Buffer
			err := DownloadContent(t.Context(), grant, &destination)
			if fixture.wantError != "" {
				if err == nil || err.Error() != fixture.wantError || uint64(destination.Len()) > grant.SizeBytes {
					t.Fatalf("expected rejection at or before the authorized size, got %v", err)
				}
				return
			}
			if err != nil || destination.String() != fixture.expectedBody {
				t.Fatalf("expected an exact copy, got %v", err)
			}
		})
	}
}

func TestDownloadContentRejectsRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/signed" {
			t.Error("download followed a redirect")
			return
		}
		http.Redirect(writer, request, "/other", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	grant := DownloadGrant{URL: server.URL + "/signed?test=signed", ExpiresAt: time.Now().Add(time.Minute)}
	var destination bytes.Buffer
	if err := DownloadContent(t.Context(), grant, &destination); err == nil || destination.Len() != 0 {
		t.Fatalf("redirect was accepted, got %v", err)
	}
}
