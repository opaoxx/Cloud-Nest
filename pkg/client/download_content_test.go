package client

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
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

func TestDownloadContentFromValidatesRangeBeforeOpeningWriter(t *testing.T) {
	for _, fixture := range []struct {
		name, contentRange, body, contentLength string
		status                                  int
		start                                   int64
	}{
		{"remaining bytes", "bytes 2-4/5", "cde", "3", 206, 2},
		{"range ignored", "", "abcde", "5", 200, 0},
		{"wrong start", "bytes 1-4/5", "bcde", "4", 206, -1},
		{"wrong end", "bytes 2-3/5", "cd", "2", 206, -1},
		{"wrong total", "bytes 2-4/6", "cde", "3", 206, -1},
		{"wrong length", "bytes 2-4/5", "cd", "2", 206, -1},
		{"partial full response", "bytes 2-4/5", "cde", "3", 200, -1},
		{"out of range", "bytes */5", "", "0", 416, -1},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Header.Get("Range") != "bytes=2-" || request.Header.Get("Authorization") != "" ||
					request.Header.Get("Accept-Encoding") != "identity" || request.URL.RawQuery != "sig=a%2Bb" {
					t.Error("download range request changed the signed URL or headers")
				}
				writer.Header().Set("Content-Length", fixture.contentLength)
				if fixture.contentRange != "" {
					writer.Header().Set("Content-Range", fixture.contentRange)
				}
				writer.WriteHeader(fixture.status)
				fmt.Fprint(writer, fixture.body)
			}))
			defer server.Close()
			grant := DownloadGrant{URL: server.URL + "/object?sig=a%2Bb", ExpiresAt: time.Now().Add(time.Minute), SizeBytes: 5}
			var destination bytes.Buffer
			started := int64(-1)
			err := DownloadContentFrom(t.Context(), grant, 2, func(start int64) (io.Writer, error) {
				started = start
				return &destination, nil
			})
			if fixture.start < 0 {
				if err == nil || started != -1 || destination.Len() != 0 {
					t.Fatalf("invalid headers opened the writer: %d, %q, %v", started, &destination, err)
				}
			} else if err != nil || started != fixture.start || destination.String() != fixture.body {
				t.Fatalf("incorrect write: %d, %q, %v", started, &destination, err)
			}
		})
	}
}
