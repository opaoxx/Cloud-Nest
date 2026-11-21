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
	receiptList, err := client.ListUploadParts(ctx, key, uploadID, 1)
	if err != nil || receiptList == nil || len(receiptList) != 0 {
		t.Fatal("错误请求改变了隔离会话中的分片", err)
	}

	// 3. 先写尾片再写第一片，逐页核对编号、实际长度、ETag 和 SHA-256。
	tailETag := putMultipartProbePart(t, ctx, httpClient, signed, checksum, tail, http.StatusOK, "")
	first := []byte{0x42, 0x47}
	firstChecksum := multipartProbeSHA256(first)
	firstLink, err := client.SignUploadPartURL(ctx, key, uploadID, 1, uint64(len(first)), firstChecksum, taskDeadline)
	if err != nil || firstLink == nil {
		t.Fatal("隔离第一片签发失败", err)
	}
	firstURL, err := url.Parse(firstLink.URL)
	if err != nil || firstURL.Host != settings.PublicEndpointURL.Host || firstURL.Scheme != settings.PublicEndpointURL.Scheme ||
		firstURL.Path != "/"+bucket.name+"/"+key || firstURL.Query().Get("uploadId") != uploadID {
		t.Fatal("签名地址不属于本轮隔离会话")
	}
	firstETag := putMultipartProbePart(t, ctx, httpClient, firstURL, firstChecksum, first, http.StatusOK, "")
	firstPage, err := client.ListUploadPartsPage(ctx, key, uploadID, 0, 1)
	if err != nil || firstPage == nil || len(firstPage.Parts) != 1 || firstPage.NextMarker != 1 {
		t.Fatal("第一页没有返回预期续页标记", err)
	}
	part := firstPage.Parts[0]
	if part.PartNumber != 1 || part.SizeBytes != 2 || part.ETag != firstETag || part.ChecksumSHA256 != firstChecksum {
		t.Fatal("第一页分片回执与写入结果不符")
	}
	lastPage, err := client.ListUploadPartsPage(ctx, key, uploadID, firstPage.NextMarker, 1)
	if err != nil || lastPage == nil || len(lastPage.Parts) != 1 || lastPage.NextMarker != 0 {
		t.Fatal("第二页没有正常结束", err)
	}
	part = lastPage.Parts[0]
	if part.PartNumber != 2 || part.SizeBytes != 1 || part.ETag != tailETag || part.ChecksumSHA256 != checksum {
		t.Fatal("尾片回执与写入结果不符")
	}
	receiptList, err = client.ListUploadParts(ctx, key, uploadID, 1)
	if err != nil || len(receiptList) != 2 || receiptList[0] != firstPage.Parts[0] || receiptList[1] != lastPage.Parts[0] {
		t.Fatal("整轮分页未依次交付两页的实际回执", err)
	}
	t.Logf("独占 MinIO %s。空会话及完整分页均通过，标记 0 → 1 → 末页，两片回执一致", container[:12])
}
