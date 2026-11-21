package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/config"
)

const multipartProbeChecksumHeader = "x-amz-checksum-sha256"

type multipartProbe struct {
	core     minio.Core
	bucket   string
	key      string
	uploadID string
	endpoint *url.URL
}

func TestServerB4MultipartProbeIntegration(t *testing.T) {
	cli := webFixtureBrowserCLI(t, "CLOUDNEST_B4_MULTIPART_PLAYWRIGHT_CLI")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	containerID := startIsolatedWebMinIO(t, ctx)
	settings, err := config.LoadStorageConfig()
	if err != nil {
		t.Fatal(err)
	}
	bucket := prepareServerUploadContentBucket(t, ctx)
	probe := multipartProbe{core: minio.Core{Client: bucket.client}, bucket: bucket.name, key: "b4/synthetic.bin", endpoint: settings.EndpointURL}
	probe.uploadID, err = probe.core.NewMultipartUpload(ctx, probe.bucket, probe.key, minio.PutObjectOptions{
		UserMetadata: map[string]string{"x-amz-checksum-algorithm": "SHA256"},
	})
	if err != nil || probe.uploadID == "" {
		t.Fatal("隔离 Multipart 初始化失败", minio.ToErrorResponse(err).Code)
	}
	t.Cleanup(func() {
		cleanupContext, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		err := probe.core.AbortMultipartUpload(cleanupContext, probe.bucket, probe.key, probe.uploadID)
		if err != nil && minio.ToErrorResponse(err).Code != "NoSuchUpload" {
			t.Error("清理本次 Multipart 会话失败", minio.ToErrorResponse(err).Code)
		}
	})
	t.Logf("专用 MinIO 容器 %s，回环地址 %s，版本化桶 %s", containerID, probe.endpoint, probe.bucket)
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(client.CloseIdleConnections)

	// 1. 分别验证签名长度与签名摘要。错误请求结束后，存储端仍无分片。
	const partSize = 16 << 20
	original := bytes.Repeat([]byte{0x11}, partSize)
	replacement := bytes.Repeat([]byte{0x22}, partSize)
	tail := []byte{0x33}
	originalSum, replacementSum, tailSum := multipartProbeSHA256(original), multipartProbeSHA256(replacement), multipartProbeSHA256(tail)
	short := original[:len(original)-1]
	shortSum := multipartProbeSHA256(short)
	lengthURL := probe.sign(t, ctx, 1, partSize, shortSum)
	putMultipartProbePart(t, ctx, client, lengthURL, shortSum, short, http.StatusForbidden, "SignatureDoesNotMatch")
	originalURL := probe.sign(t, ctx, 1, len(original), originalSum)
	putMultipartProbePart(t, ctx, client, originalURL, originalSum, replacement, http.StatusBadRequest, "XAmzContentChecksumMismatch")
	page, err := probe.core.ListObjectParts(ctx, probe.bucket, probe.key, probe.uploadID, 0, 1)
	if err != nil || page.IsTruncated || len(page.ObjectParts) != 0 {
		t.Fatal("拒绝长度或摘要不符的请求后不应出现已保存分片", minio.ToErrorResponse(err).Code)
	}

	// 2. Go 上传初版和尾片。Edge 以固定长度 Blob 覆盖第 1 片，读回跨域 ETag。
	oldETag := putMultipartProbePart(t, ctx, client, originalURL, originalSum, original, http.StatusOK, "")
	tailURL := probe.sign(t, ctx, 2, len(tail), tailSum)
	tailETag := putMultipartProbePart(t, ctx, client, tailURL, tailSum, tail, http.StatusOK, "")
	first := probe.readPartPage(t, ctx, 0, 1, len(original), oldETag, originalSum)
	if !first.IsTruncated || first.NextPartNumberMarker != 1 {
		t.Fatal("隔离会话未返回预期的分片续页")
	}
	replacementURL := probe.sign(t, ctx, 1, len(replacement), replacementSum)
	newETag := runMultipartBrowserProbe(t, ctx, cli, client, replacementURL, replacementSum, len(replacement), 0x22)
	if oldETag == newETag {
		t.Fatal("覆盖分片后的浏览器 ETag 应发生变化")
	}

	// 3. 核对两页的最新回执，先验证旧回执被拒，再用新回执完成并读回对象版本。
	first = probe.readPartPage(t, ctx, 0, 1, len(replacement), newETag, replacementSum)
	if !first.IsTruncated || first.NextPartNumberMarker != 1 {
		t.Fatal("覆盖分片后的分页标记不符")
	}
	last := probe.readPartPage(t, ctx, first.NextPartNumberMarker, 2, len(tail), tailETag, tailSum)
	if last.IsTruncated {
		t.Fatal("读到尾片后仍出现续页")
	}
	parts := []minio.CompletePart{
		{PartNumber: 1, ETag: oldETag, ChecksumSHA256: originalSum},
		{PartNumber: 2, ETag: tailETag, ChecksumSHA256: tailSum},
	}
	if _, err := probe.core.CompleteMultipartUpload(ctx, probe.bucket, probe.key, probe.uploadID, parts, minio.PutObjectOptions{}); minio.ToErrorResponse(err).Code != "InvalidPart" {
		t.Fatal("旧分片回执应被拒绝，实际错误码", minio.ToErrorResponse(err).Code)
	}
	parts[0] = minio.CompletePart{PartNumber: 1, ETag: newETag, ChecksumSHA256: replacementSum}
	completed, err := probe.core.CompleteMultipartUpload(ctx, probe.bucket, probe.key, probe.uploadID, parts, minio.PutObjectOptions{})
	if err != nil || completed.VersionID == "" || completed.VersionID == "null" {
		t.Fatal("新回执应完成版本化对象", minio.ToErrorResponse(err).Code)
	}
	reader, stored, _, err := probe.core.GetObject(ctx, probe.bucket, probe.key, minio.GetObjectOptions{VersionID: completed.VersionID})
	if err != nil {
		t.Fatal("读取本轮完成对象失败", minio.ToErrorResponse(err).Code)
	}
	defer reader.Close()
	expectedHash := sha256.New()
	expectedHash.Write(replacement)
	expectedHash.Write(tail)
	downloadedHash := sha256.New()
	expectedBytes := int64(len(replacement) + len(tail))
	count, err := io.Copy(downloadedHash, io.LimitReader(reader, expectedBytes+1))
	if err != nil || count != expectedBytes || stored.Size != expectedBytes || stored.VersionID != completed.VersionID ||
		!bytes.Equal(downloadedHash.Sum(nil), expectedHash.Sum(nil)) {
		t.Fatal("完成对象的版本、大小或整文件摘要不符", count, err)
	}
	t.Logf("ListParts 每页 1 片，旧 ETag %s 完成返回 InvalidPart，新 ETag %s 与尾片 ETag %s 完成版本 %s",
		oldETag, newETag, tailETag, completed.VersionID)
	t.Logf("指定版本读回 %d B，整文件 SHA-256 %x", count, downloadedHash.Sum(nil))
}

func multipartProbeSHA256(payload []byte) string {
	digest := sha256.Sum256(payload)
	return base64.StdEncoding.EncodeToString(digest[:])
}

func (probe multipartProbe) sign(t *testing.T, ctx context.Context, number, size int, checksum string) *url.URL {
	t.Helper()
	headers := make(http.Header)
	headers.Set("Content-Length", strconv.Itoa(size))
	headers.Set(multipartProbeChecksumHeader, checksum)
	query := url.Values{"uploadId": {probe.uploadID}, "partNumber": {strconv.Itoa(number)}}
	signed, err := probe.core.PresignHeader(ctx, http.MethodPut, probe.bucket, probe.key, 5*time.Minute, query, headers)
	if err != nil || signed.Scheme != probe.endpoint.Scheme || signed.Host != probe.endpoint.Host || signed.User != nil ||
		signed.Path != "/"+probe.bucket+"/"+probe.key || signed.Query().Get("X-Amz-SignedHeaders") != "content-length;host;x-amz-checksum-sha256" {
		t.Fatal("分片 URL 未绑定隔离目标和预期签名头")
	}
	return signed
}

func putMultipartProbePart(t *testing.T, ctx context.Context, client *http.Client, signed *url.URL, checksum string, body []byte, wantStatus int, wantCode string) string {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, signed.String(), bytes.NewReader(body))
	if err != nil {
		t.Fatal("无法构造隔离分片请求")
	}
	request.Header.Set(multipartProbeChecksumHeader, checksum)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("隔离 MinIO 分片请求失败")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		var result struct{ Code string }
		_ = xml.NewDecoder(io.LimitReader(response.Body, 8192)).Decode(&result)
		if response.StatusCode != wantStatus || result.Code != wantCode {
			t.Fatalf("分片拒绝不符，得到 %d %s，预期 %d %s", response.StatusCode, result.Code, wantStatus, wantCode)
		}
		t.Logf("隔离分片 PUT 返回 %d %s", response.StatusCode, result.Code)
		return ""
	}
	if wantStatus != http.StatusOK || response.Header.Get(multipartProbeChecksumHeader) != checksum {
		t.Fatal("隔离分片 PUT 意外成功或 SHA-256 回执不符")
	}
	return multipartProbeETag(t, response.Header.Get("ETag"))
}

func multipartProbeETag(t *testing.T, raw string) string {
	t.Helper()
	etag := raw
	if strings.HasPrefix(etag, `"`) && strings.HasSuffix(etag, `"`) && len(etag) >= 2 {
		etag = etag[1 : len(etag)-1]
	}
	if !regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`).MatchString(etag) {
		t.Fatal("存储回执不满足草案中的 ETag 格式")
	}
	return etag
}

func (probe multipartProbe) readPartPage(t *testing.T, ctx context.Context, marker, number, size int, etag, checksum string) minio.ListObjectPartsResult {
	t.Helper()
	page, err := probe.core.ListObjectParts(ctx, probe.bucket, probe.key, probe.uploadID, marker, 1)
	if err != nil || page.PartNumberMarker != marker || len(page.ObjectParts) != 1 {
		t.Fatal("ListParts 分页响应不符", minio.ToErrorResponse(err).Code)
	}
	part := page.ObjectParts[0]
	if part.PartNumber != number || part.Size != int64(size) || multipartProbeETag(t, part.ETag) != etag || part.ChecksumSHA256 != checksum {
		t.Fatal("ListParts 没有返回该编号的最新 ETag、长度和 SHA-256", number)
	}
	return page
}

func runMultipartBrowserProbe(t *testing.T, ctx context.Context, cli string, client *http.Client, signed *url.URL, checksum string, size, fill int) string {
	t.Helper()
	// 1. 回环页面交付当轮签名和合成字节参数，浏览器通过独立入口回传读取到的头。
	receipts := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<!doctype html><title>CloudNest B4 multipart probe</title>")
	})
	mux.HandleFunc("GET /plan", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{"url": signed.String(), "checksum": checksum, "size": size, "fill": fill})
	})
	mux.HandleFunc("POST /receipt", func(w http.ResponseWriter, r *http.Request) {
		var etag string
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 256)).Decode(&etag) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		select {
		case receipts <- etag:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusConflict)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	// 2. 对同一回环来源预检，再让真实浏览器完成跨域请求和回执回传。
	preflight, err := http.NewRequestWithContext(ctx, http.MethodOptions, signed.String(), nil)
	if err != nil {
		t.Fatal("无法构造隔离 CORS 预检")
	}
	preflight.Header.Set("Origin", server.URL)
	preflight.Header.Set("Access-Control-Request-Method", http.MethodPut)
	preflight.Header.Set("Access-Control-Request-Headers", multipartProbeChecksumHeader)
	response, err := client.Do(preflight)
	if err != nil {
		t.Fatal("隔离 CORS 预检失败")
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent || response.Header.Get("Access-Control-Allow-Origin") != server.URL ||
		!strings.Contains(response.Header.Get("Access-Control-Allow-Methods"), "PUT") ||
		!strings.Contains(strings.ToLower(response.Header.Get("Access-Control-Allow-Headers")), multipartProbeChecksumHeader) {
		t.Fatal("固定 MinIO 的跨域分片预检不符", response.StatusCode)
	}
	runWebBrowserProbe(t, ctx, cli, server.URL, "b4_multipart_probe.js", "CN550 browser ETag received")
	select {
	case etag := <-receipts:
		t.Log("隔离 CORS 预检返回 204，Edge Blob 直传返回 200，页面读取到 ETag")
		return multipartProbeETag(t, etag)
	default:
		t.Fatal("回环页面未收到浏览器可读取的 ETag")
		return ""
	}
}
