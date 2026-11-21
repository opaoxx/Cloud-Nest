package storage

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const signingTestChecksum = "TgdAhWK+24tgzgXB3s/jrRa3IjCWfeAfZAt+Rym0n84="

func TestSignUploadPartURLUsesPublicEndpoint(t *testing.T) {
	var requests atomic.Int32
	internal := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		switch {
		case request.Method == http.MethodGet && request.URL.Query().Has("location"):
			_, _ = io.WriteString(writer, "<LocationConstraint>us-west-2</LocationConstraint>")
		case request.Method == http.MethodHead:
			writer.WriteHeader(http.StatusOK)
		default:
			t.Error("签发发生了意外的内部请求", request.Method)
			writer.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(internal.Close)
	public := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		t.Error("签发访问了客户端地址")
		writer.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(public.Close)
	settings := storageTestConfig(t, internal.URL)
	var err error
	settings.PublicEndpointURL, err = url.Parse(public.URL)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(t.Context(), settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	afterStartup := requests.Load()
	const key = "users/7/objects/fixture +%#?.bin"
	const uploadID = "fixture+A/="
	for _, test := range []struct {
		name        string
		remaining   time.Duration
		maxSeconds  int
		expectLimit bool
	}{
		{name: "five-minute cap", remaining: time.Hour, maxSeconds: 300, expectLimit: true},
		{name: "task deadline", remaining: 8*time.Second + 500*time.Millisecond, maxSeconds: 8},
	} {
		t.Run(test.name, func(t *testing.T) {
			deadline := time.Now().Add(test.remaining)
			result, err := client.SignUploadPartURL(t.Context(), key, uploadID, 10_000, 1, signingTestChecksum, deadline)
			if err != nil || result == nil {
				t.Fatal("签发失败", err)
			}
			signed, err := url.Parse(result.URL)
			if err != nil || signed.Scheme != settings.PublicEndpointURL.Scheme || signed.Host != settings.PublicEndpointURL.Host ||
				signed.Path != "/cloudnest/"+key || signed.User != nil || signed.Fragment != "" {
				t.Fatal("签名未绑定客户端主机及固定对象键")
			}
			query := signed.Query()
			if query.Get("uploadId") != uploadID || query.Get("partNumber") != "10000" ||
				query.Get("X-Amz-SignedHeaders") != "content-length;host;x-amz-checksum-sha256" || len(query.Get("X-Amz-Signature")) != 64 {
				t.Fatal("签名未绑定会话、编号和校验头")
			}
			signedAt, timeError := time.Parse(sigV4SigningTimeLayout, query.Get("X-Amz-Date"))
			seconds, expiryError := strconv.Atoi(query.Get("X-Amz-Expires"))
			if timeError != nil || expiryError != nil || seconds < 1 || seconds > test.maxSeconds ||
				(test.expectLimit && seconds != test.maxSeconds) || !result.ExpiresAt.Equal(signedAt.Add(time.Duration(seconds)*time.Second)) ||
				result.ExpiresAt.After(deadline) || requests.Load() != afterStartup {
				t.Fatal("签名截止时间不符，或签发发起了网络请求")
			}
		})
	}
}

func TestSignUploadPartURLRejectsBadInput(t *testing.T) {
	client := downloadObjectTestClient(t, func(writer http.ResponseWriter, _ *http.Request) {
		t.Error("参数校验触发了存储调用")
		writer.WriteHeader(http.StatusBadRequest)
	})
	for _, test := range []struct {
		name, key, id, checksum string
		number                  uint32
		size                    uint64
		remaining               time.Duration
		want                    error
	}{
		{"invalid key", "object\n", "session", signingTestChecksum, 1, 1, time.Minute, domain.ErrInvalidStoredObjectData},
		{"empty id", "object", "", signingTestChecksum, 1, 1, time.Minute, domain.ErrInvalidUploadTaskData},
		{"zero number", "object", "session", signingTestChecksum, 0, 1, time.Minute, domain.ErrInvalidUploadPartNumber},
		{"oversized number", "object", "session", signingTestChecksum, 10_001, 1, time.Minute, domain.ErrInvalidUploadPartNumber},
		{"zero bytes", "object", "session", signingTestChecksum, 1, 0, time.Minute, domain.ErrInvalidUploadTaskData},
		{"too many bytes", "object", "session", signingTestChecksum, 1, (5 << 30) + 1, time.Minute, domain.ErrInvalidUploadTaskData},
		{"bad padding bits", "object", "session", signingTestChecksum[:42] + "5=", 1, 1, time.Minute, domain.ErrInvalidUploadTaskData},
		{"missing padding", "object", "session", signingTestChecksum[:43], 1, 1, time.Minute, domain.ErrInvalidUploadTaskData},
		{"sub-second lifetime", "object", "session", signingTestChecksum, 1, 1, 500 * time.Millisecond, domain.ErrUploadTaskNotReady},
		{"expired task", "object", "session", signingTestChecksum, 1, 1, -time.Second, domain.ErrUploadTaskNotReady},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := client.SignUploadPartURL(t.Context(), test.key, test.id, test.number, test.size, test.checksum, time.Now().Add(test.remaining))
			if got != nil || !errors.Is(err, test.want) {
				t.Fatal("错误签名参数未按预期拒绝", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if got, err := client.SignUploadPartURL(ctx, "object", "session", 1, 1, signingTestChecksum, time.Now().Add(time.Minute)); got != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("签发未保留取消原因", err)
	}
}
