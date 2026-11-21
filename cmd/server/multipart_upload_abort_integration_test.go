package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/storage"
)

func TestServerMultipartUploadAbortIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MULTIPART_ABORT_STORAGE_TEST") != "1" {
		t.Skip("显式启用后创建本轮专用 MinIO 容器和版本桶")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	containerID := startIsolatedWebMinIO(t, ctx)
	t.Logf("专用 MinIO %s，回环地址 %s", containerID[:12], os.Getenv("CLOUDNEST_S3_ENDPOINT"))
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

	// 1. 在独占桶的同一合成键准备已存在的版本和两场分片会话。
	const key = "b4/abort synthetic+%.bin"
	const existing = "synthetic original object"
	version, err := bucket.client.PutObject(ctx, bucket.name, key, strings.NewReader(existing), int64(len(existing)), minio.PutObjectOptions{})
	if err != nil || version.VersionID == "" || version.VersionID == "null" {
		t.Fatal("未建立合成对象版本", minio.ToErrorResponse(err).Code)
	}
	core := minio.Core{Client: bucket.client}
	httpClient := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(httpClient.CloseIdleConnections)
	createSession := func(payload []byte) string {
		t.Helper()
		uploadID, err := client.CreateMultipartUpload(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cleanupContext, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			if err := core.AbortMultipartUpload(cleanupContext, bucket.name, key, uploadID); err != nil && minio.ToErrorResponse(err).Code != "NoSuchUpload" {
				t.Error("专用会话清理失败", minio.ToErrorResponse(err).Code)
			}
		})
		probe := multipartProbe{core: core, bucket: bucket.name, key: key, uploadID: uploadID, endpoint: settings.EndpointURL}
		digest := multipartProbeSHA256(payload)
		signed := probe.sign(t, ctx, 1, len(payload), digest)
		etag := putMultipartProbePart(t, ctx, httpClient, signed, digest, payload, http.StatusOK, "")
		page, err := client.ListUploadPartsPage(ctx, key, uploadID, 0, 1)
		if err != nil || page == nil || len(page.Parts) != 1 || page.Parts[0].ETag != etag || page.Parts[0].SizeBytes != uint64(len(payload)) {
			t.Fatal("未读到中止前的合成分片", err)
		}
		return uploadID
	}
	targetSession := createSession([]byte{0x11})
	controlPayload := []byte{0x22, 0x33}
	controlSession := createSession(controlPayload)
	if targetSession == controlSession {
		t.Fatal("夹具必须有两个独立会话")
	}

	// 2. 中止目标会话，核对原会话缺失和存储对重复中止的结果。
	if err := client.AbortMultipartUpload(ctx, key, targetSession); err != nil {
		t.Fatal("首次中止应收到存储确认", err)
	}
	_, err = core.ListObjectParts(ctx, bucket.name, key, targetSession, 0, 1)
	response := minio.ToErrorResponse(err)
	if response.StatusCode != http.StatusNotFound || response.Code != "NoSuchUpload" || response.RequestID == "" {
		t.Fatal("中止后仍未得到明确的原会话缺失", response.StatusCode, response.Code)
	}
	switch err := client.AbortMultipartUpload(ctx, key, targetSession); {
	case err == nil:
		t.Log("首次中止确认，ListParts 返回 404 NoSuchUpload，重复中止仍收到确认")
	case errors.Is(err, domain.ErrUploadPartSessionMissing):
		t.Log("首次中止确认，ListParts 和重复中止均返回 404 NoSuchUpload")
	default:
		t.Fatal("重复中止未收到确认或会话缺失结果", err)
	}

	// 3. 确认中止仅作用于绑定会话，同键对照会话仍可读，已有版本仍是当前对象。
	page, err := client.ListUploadPartsPage(ctx, key, controlSession, 0, 1)
	if err != nil || page == nil || len(page.Parts) != 1 || page.Parts[0].ChecksumSHA256 != multipartProbeSHA256(controlPayload) {
		t.Fatal("同键的对照会话未保留原分片", err)
	}
	object, err := client.StatMultipartUploadObject(ctx, key)
	if err != nil || object == nil || object.StorageVersionID == nil || *object.StorageVersionID != version.VersionID ||
		object.StorageETag == nil || *object.StorageETag != version.ETag || object.SizeBytes != uint64(len(existing)) {
		t.Fatal("同键的已存在对象版本未保留", err)
	}
	t.Log("同键对照会话仍有原分片，已存在对象的版本、ETag 和大小一致")
}
