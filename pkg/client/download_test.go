package client

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCreateDownloadGrantPreservesSelectedRevision(t *testing.T) {
	const entryID = "18446744073709551613"
	const revision = "18446744073709551614"
	const objectID = "18446744073709551615"
	const signedURL = "http://storage.invalid/test%2Fobject?part=a%2Bb&sig=%2Ftest"
	const digest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	for _, fixture := range []struct {
		name    string
		change  map[string]any
		wantErr string
	}{
		{name: "empty file"},
		{name: "different revision", change: map[string]any{"revision": "2"}, wantErr: "服务返回的下载授权有误"},
		{name: "absent size", change: map[string]any{"size_bytes": nil}, wantErr: "服务返回的下载授权有误"},
		{name: "unsafe size", change: map[string]any{"size_bytes": uint64(1 << 53)}, wantErr: "服务返回的下载授权有误"},
		{name: "invalid digest", change: map[string]any{"sha256": strings.ToUpper(digest)}, wantErr: "服务返回的下载授权有误"},
		{name: "invalid object", change: map[string]any{"object_id": "0"}, wantErr: "服务返回的下载授权有误"},
		{name: "unsafe URL", change: map[string]any{"url": "https://user:pass@storage.invalid/object"}, wantErr: "服务返回的下载授权有误"},
		{name: "wrong purpose", change: map[string]any{"disposition": "inline; filename=sample"}, wantErr: "服务返回的下载授权有误"},
		{name: "expired link", change: map[string]any{"expires_at": "2000-01-01T00:00:00Z"}, wantErr: "下载授权链接已过期，请重新获取"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			data := map[string]any{
				"url": signedURL, "expires_at": time.Now().UTC().Add(time.Minute), "object_id": objectID,
				"revision": revision, "size_bytes": 0, "sha256": digest,
				"disposition": "attachment; filename=sample; filename*=UTF-8''%E8%B5%84%E6%96%99.txt",
			}
			for field, value := range fixture.change {
				data[field] = value
			}
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodPost || request.URL.RequestURI() != "/api/v1/entries/"+entryID+"/download" ||
					request.Header.Get("Authorization") != "Bearer sample" || request.Header.Get("Content-Type") != "application/json" ||
					request.Header.Get("Idempotency-Key") != "" {
					t.Error("request did not match the download authorization contract")
				}
				body, err := io.ReadAll(request.Body)
				if err != nil || string(body) != `{"expected_revision":"`+revision+`","purpose":"download"}` {
					t.Error("request did not preserve the selected revision and purpose")
				}
				if err := json.NewEncoder(writer).Encode(map[string]any{"data": data}); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			grant, err := CreateDownloadGrant(t.Context(), server.URL, "sample", entryID, revision)
			if fixture.wantErr != "" {
				if err == nil || err.Error() != fixture.wantErr || grant != (DownloadGrant{}) {
					t.Fatalf("unexpected authorization error: %v", err)
				}
			} else if err != nil || grant.URL != signedURL || grant.ObjectID != objectID || grant.Revision != revision ||
				grant.SizeBytes != 0 || grant.SHA256 != digest || grant.ExpiresAt.IsZero() {
				t.Fatalf("valid grant was not preserved: %v", err)
			}
		})
	}
}

func TestCreateDownloadGrantClassifiesServerRejection(t *testing.T) {
	for _, fixture := range []struct {
		status  int
		code    string
		wantErr string
	}{
		{http.StatusUnauthorized, "TOKEN_EXPIRED", ErrAccessTokenExpired.Error()},
		{http.StatusConflict, "REVISION_CONFLICT", "文件修订已变化，请刷新目录后重新选择"},
		{http.StatusConflict, "TARGET_UNAVAILABLE", "文件内容当前不可用"},
		{http.StatusNotFound, "ENTRY_NOT_FOUND", "文件不存在或已不可访问，请刷新目录"},
	} {
		t.Run(fixture.code, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.WriteHeader(fixture.status)
				_, _ = io.WriteString(writer, `{"error":{"code":"`+fixture.code+`","details":{"current_revision":"4"}}}`)
			}))
			defer server.Close()
			grant, err := CreateDownloadGrant(t.Context(), server.URL, "sample", "12", "3")
			if err == nil || err.Error() != fixture.wantErr || grant != (DownloadGrant{}) ||
				errors.Is(err, ErrAccessTokenExpired) != (fixture.code == "TOKEN_EXPIRED") {
				t.Fatalf("unexpected authorization error: %v", err)
			}
		})
	}
}
