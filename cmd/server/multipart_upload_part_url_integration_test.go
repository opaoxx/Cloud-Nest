package main

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/storage"
)

func TestServerMultipartUploadPartURLIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MULTIPART_PART_URL_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_MULTIPART_PART_URL_TEST=1 后使用独占 MinIO 验证单片签名")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()

	// 1. 使用正式适配器创建隔离会话并签发指定尾片的 PUT URL。
	container := startIsolatedWebMinIO(t, ctx)
	t.Setenv("CLOUDNEST_STORAGE_PROVIDER", "local_minio")
	bucket := prepareServerUploadContentBucket(t, ctx)
	settings, err := config.LoadStorageConfig()
	if err != nil {
		t.Fatal(err)
	}
	client, err := storage.NewClient(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	const key = "users/7/objects/part-signing-fixture"
	uploadID, err := client.CreateMultipartUpload(ctx, key)
	if err != nil {
		t.Fatal("创建隔离会话失败", err)
	}
	core := minio.Core{Client: bucket.client}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if err := core.AbortMultipartUpload(cleanup, bucket.name, key, uploadID); err != nil {
			t.Error("清理本次会话失败", minio.ToErrorResponse(err).Code)
		}
	})
	tail := []byte{0x33}
	checksum := multipartProbeSHA256(tail)
	taskDeadline := time.Now().Add(35 * time.Second)
	result, err := client.SignUploadPartURL(ctx, key, uploadID, 2, uint64(len(tail)), checksum, taskDeadline)
	if err != nil || result == nil || result.ExpiresAt.After(taskDeadline) {
		t.Fatal("隔离单片签发失败", err)
	}
	signed, err := url.Parse(result.URL)
	if err != nil || signed.Host != settings.PublicEndpointURL.Host {
		t.Fatal("签名地址不属于本轮隔离服务")
	}
	httpClient := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(httpClient.CloseIdleConnections)

	// 2. 存储拒绝长度、校验头和正文不匹配的请求，保持零分片。
	putMultipartProbePart(t, ctx, httpClient, signed, checksum, []byte{0x33, 0x34}, http.StatusForbidden, "SignatureDoesNotMatch")
	other := []byte{0x34}
	putMultipartProbePart(t, ctx, httpClient, signed, multipartProbeSHA256(other), other, http.StatusForbidden, "SignatureDoesNotMatch")
	putMultipartProbePart(t, ctx, httpClient, signed, checksum, other, http.StatusBadRequest, "XAmzContentChecksumMismatch")
	page, err := core.ListObjectParts(ctx, bucket.name, key, uploadID, 0, 1)
	if err != nil || page.IsTruncated || len(page.ObjectParts) != 0 {
		t.Fatal("错误请求改变了隔离会话中的分片", minio.ToErrorResponse(err).Code)
	}

	// 3. 正确尾片上传后，按会话读回编号、长度、ETag 和 SHA-256。
	etag := putMultipartProbePart(t, ctx, httpClient, signed, checksum, tail, http.StatusOK, "")
	probe := multipartProbe{core: core, bucket: bucket.name, key: key, uploadID: uploadID}
	if result := probe.readPartPage(t, ctx, 0, 2, len(tail), etag, checksum); result.IsTruncated {
		t.Fatal("本轮上传出现了多余分片")
	}
	t.Logf("独占 MinIO %s。长度、校验头和正文错误均被拒绝，正确第 2 片的 1 字节回执一致", container[:12])
}
