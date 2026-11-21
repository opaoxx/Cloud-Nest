package main

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/storage"
)

func TestServerMultipartUploadInitializationIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MULTIPART_INITIALIZATION_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_MULTIPART_INITIALIZATION_TEST=1 后使用独占 MinIO 验证初始化")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	container := startIsolatedWebMinIO(t, ctx)
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
	const key = "users/7/objects/multipart-initialization-fixture"

	// 1. 使用真实适配器创建，再按原对象键和返回 ID 核对未完成会话。
	id, err := client.CreateMultipartUpload(ctx, key)
	if err != nil || id == "" {
		t.Fatal("独占桶初始化失败", err)
	}
	core := minio.Core{Client: bucket.client}
	t.Cleanup(func() {
		cleanupContext, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if err := core.AbortMultipartUpload(cleanupContext, bucket.name, key, id); err != nil {
			t.Error("清理本轮测试会话失败", minio.ToErrorResponse(err).Code)
		}
	})
	page, err := core.ListObjectParts(ctx, bucket.name, key, id, 0, 1)
	if err != nil || page.UploadID != id || page.Bucket != bucket.name || page.Key != key ||
		page.ChecksumAlgorithm != "SHA256" || page.IsTruncated || len(page.ObjectParts) != 0 {
		t.Fatal("初始化后的空会话、位置或校验算法不符", minio.ToErrorResponse(err).Code)
	}

	// 2. 验证预取消无新增会话，桶内尚无已提交对象版本。
	cancelled, stop := context.WithCancel(ctx)
	stop()
	if id, err := client.CreateMultipartUpload(cancelled, key); id != "" || !errors.Is(err, context.Canceled) {
		t.Fatal("预取消初始化未按预期拒绝", err)
	}
	incomplete, err := core.ListMultipartUploads(ctx, bucket.name, key, "", "", "", 10)
	if err != nil || incomplete.IsTruncated || len(incomplete.Uploads) != 1 ||
		incomplete.Uploads[0].Key != key || incomplete.Uploads[0].UploadID != id {
		t.Fatal("本轮隔离桶未完成会话与预期不符", minio.ToErrorResponse(err).Code)
	}
	for item := range bucket.client.ListObjects(ctx, bucket.name, minio.ListObjectsOptions{Recursive: true, WithVersions: true}) {
		t.Fatal("初始化后隔离桶不应包含已提交对象", minio.ToErrorResponse(item.Err).Code)
	}
	t.Logf("隔离 MinIO %s，端点 %s，桶 %s，确认一条 SHA256 未完成会话与零分片", container, settings.EndpointURL, bucket.name)
}
