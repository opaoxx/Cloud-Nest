package storage

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestSignDownloadURL(t *testing.T) {
	// 1. 桶区域从内部地址取得。独立的 HTTPS 访问地址不参与元数据查询。
	var internalRequests atomic.Int32
	internal := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		internalRequests.Add(1)
		switch {
		case request.Method == http.MethodGet && request.URL.Query().Has("location"):
			_, _ = io.WriteString(writer, "<LocationConstraint>us-west-2</LocationConstraint>")
		case request.Method == http.MethodHead:
			writer.WriteHeader(http.StatusOK)
		default:
			t.Error("内部客户端发出意外请求", request.Method)
			writer.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(internal.Close)
	public := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		t.Error("创建客户端或签名时访问了客户端地址")
		writer.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(public.Close)
	settings := storageTestConfig(t, internal.URL)
	publicEndpoint, err := url.Parse(public.URL)
	if err != nil {
		t.Fatal("解析联测客户端地址失败")
	}
	settings.PublicEndpointURL = publicEndpoint
	client, err := NewClient(t.Context(), settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	afterStartup := internalRequests.Load()

	// 2. 核对签名涉及的公开主机、精确键、可空版本和两个响应头。
	const disposition = `attachment; filename="download.txt"; filename*=UTF-8''%E5%AD%A6%E4%B9%A0.txt`
	for _, scenario := range []struct{ label, version string }{
		{"omitted version", ""},
		{"explicit null", "null"},
		{"opaque version", "old+A/="},
	} {
		t.Run(scenario.label, func(t *testing.T) {
			target := downloadObjectTestTarget()
			target.ObjectKey = "users/7/objects/name +%#?.bin"
			if scenario.version != "" {
				target.StorageVersionID = new(scenario.version)
			}
			result, err := client.SignDownloadURL(t.Context(), target, "text/plain", disposition)
			if err != nil || result == nil {
				t.Fatal("签发下载地址失败", err)
			}
			signed, err := url.Parse(result.URL)
			if err != nil || signed.Scheme != "https" || signed.Host != settings.PublicEndpointURL.Host ||
				signed.Path != "/cloudnest/"+target.ObjectKey || signed.User != nil || signed.Fragment != "" {
				t.Fatal("签名地址未绑定到客户端主机和对象键")
			}
			query := signed.Query()
			if query.Get("versionId") != scenario.version || query.Has("versionId") != (scenario.version != "") ||
				query.Get("response-content-type") != "text/plain" || query.Get("response-content-disposition") != disposition ||
				query.Get("X-Amz-Algorithm") != "AWS4-HMAC-SHA256" || query.Get("X-Amz-SignedHeaders") != "host" ||
				len(query.Get("X-Amz-Signature")) != 64 || !strings.Contains(query.Get("X-Amz-Credential"), "/us-west-2/s3/aws4_request") ||
				strings.Contains(result.URL, settings.SecretKey) {
				t.Fatal("签名参数、区域或凭证边界错误")
			}

			// 3. 到期时间从链接中的签名时间和固定分钟数计算。
			signedAt, err := time.Parse("20060102T150405Z", query.Get("X-Amz-Date"))
			if err != nil || query.Get("X-Amz-Expires") != "900" || !result.ExpiresAt.Equal(signedAt.Add(15*time.Minute)) ||
				result.ExpiresAt.Location() != time.UTC || internalRequests.Load() != afterStartup {
				t.Fatal("签名截止时间不符或签发期间发出了内部请求")
			}
		})
	}
}

func TestSignDownloadURLRejectsInvalidInput(t *testing.T) {
	client := downloadObjectTestClient(t, func(writer http.ResponseWriter, _ *http.Request) {
		t.Error("签名验证触发了元数据请求")
		writer.WriteHeader(http.StatusBadRequest)
	})
	const disposition = `attachment; filename="file.txt"; filename*=UTF-8''file.txt`
	for _, invalid := range []struct{ contentType, disposition string }{
		{"", disposition},
		{"text/plain\r\nprivate-header", disposition},
		{"text/plain", ""},
		{"text/plain", "inline\x00private-header"},
		{"text/plain", `inline; filename="文件.txt"`},
	} {
		result, err := client.SignDownloadURL(t.Context(), downloadObjectTestTarget(), invalid.contentType, invalid.disposition)
		if result != nil || err == nil || strings.Contains(err.Error(), "private-header") {
			t.Fatal("无效头值未安全拒绝", err)
		}
	}
	for _, invalid := range []*domain.EntryDownloadTarget{
		nil,
		{ObjectID: 23, UserID: 7, Provider: "local_minio", Bucket: "other-bucket", ObjectKey: "object"},
		{ObjectID: 23, UserID: 7, Provider: "local_minio", Bucket: "cloudnest", ObjectKey: "object", StorageVersionID: new("")},
	} {
		result, err := client.SignDownloadURL(t.Context(), invalid, "text/plain", disposition)
		if result != nil || !errors.Is(err, domain.ErrInvalidStoredObjectData) {
			t.Fatal("无效对象位置未安全拒绝", err)
		}
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if result, err := client.SignDownloadURL(cancelled, downloadObjectTestTarget(), "text/plain", disposition); result != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("签名未保留请求取消", err)
	}
}
