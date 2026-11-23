package client

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSendSingleUploadContentUsesRawBytes(t *testing.T) {
	for _, fixture := range []struct {
		name        string
		body        []byte
		replyTaskID string
	}{
		{name: "empty", replyTaskID: "19"},
		{name: "binary", body: []byte{0, 255, 10, 13}, replyTaskID: "19"},
		{name: "unexpected receipt", body: []byte("abc"), replyTaskID: "20"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodPut || request.URL.RequestURI() != "/api/v1/uploads/19/content" ||
					request.Header.Get("Authorization") != "Bearer sample" ||
					request.Header.Get("Content-Type") != "application/octet-stream" ||
					request.ContentLength != int64(len(fixture.body)) || len(request.TransferEncoding) != 0 {
					t.Error("request did not match the SINGLE body contract")
				}
				body, err := io.ReadAll(request.Body)
				if err != nil || !bytes.Equal(body, fixture.body) {
					t.Errorf("unexpected request body: %x, %v", body, err)
				}
				writer.WriteHeader(http.StatusAccepted)
				fmt.Fprintf(writer, `{"data":{"task_id":%q,"status":"VERIFYING"}}`, fixture.replyTaskID)
			}))
			defer server.Close()
			target := SingleUploadContentTarget{TaskID: "19", ContentPath: "/api/v1/uploads/19/content", SizeBytes: int64(len(fixture.body))}
			err := SendSingleUploadContent(t.Context(), server.URL, "sample", target, bytes.NewReader(fixture.body))
			if (err != nil) != (fixture.replyTaskID != "19") {
				t.Fatalf("unexpected receipt result: %v", err)
			}
		})
	}
}

func TestSendSingleUploadContentRejectsUnexpectedPath(t *testing.T) {
	for _, path := range []string{"/api/v1/uploads/20/content", "/api/v1/uploads/19/content?token=extra", "http://127.0.0.1:1/api/v1/uploads/19/content"} {
		target := SingleUploadContentTarget{TaskID: "19", ContentPath: path, SizeBytes: 0}
		if err := SendSingleUploadContent(t.Context(), "http://127.0.0.1:1", "sample", target, nil); err == nil || err.Error() != "无效的 SINGLE 正文目标" {
			t.Fatalf("unexpected path was not rejected before sending: %v", err)
		}
	}
}
