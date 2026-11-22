package main

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/internal/server/storage"
)

func TestServerMultipartSessionListingIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MULTIPART_SESSION_LISTING_TEST") != "1" {
		t.Skip("显式启用后创建本轮专用 MinIO 版本桶")
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
	core := minio.Core{Client: bucket.client}
	key, err := service.GenerateUploadObjectKey(7)
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := service.GenerateUploadObjectKey(8)
	if err != nil {
		t.Fatal(err)
	}
	emptyKey, err := service.GenerateUploadObjectKey(7)
	if err != nil {
		t.Fatal(err)
	}
	create := func(objectKey string) string {
		t.Helper()
		uploadID, err := client.CreateMultipartUpload(ctx, objectKey)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			if err := core.AbortMultipartUpload(cleanup, bucket.name, objectKey, uploadID); err != nil {
				t.Error("清理本轮合成会话失败", minio.ToErrorResponse(err).Code)
			}
		})
		return uploadID
	}
	// 1. 同一原键开启三条会话，另一个账号的键作为范围对照。
	wantIDs := []string{create(key), create(key), create(key)}
	otherID := create(otherKey)
	prefixOnly, err := core.ListMultipartUploads(ctx, bucket.name, "users/", "", "", "", 1)
	if err != nil || len(prefixOnly.Uploads) != 0 {
		t.Fatal("固定 MinIO 的完整对象键处理方式与预期不符", err)
	}

	// 2. 每页只取一条，沿原游标读取完整集合。
	var gotIDs []string
	var previousTime time.Time
	marker := ""
	for index := range wantIDs {
		page, err := client.ListMultipartUploadSessionsPage(ctx, key, marker, 1)
		if err != nil || page == nil || len(page.Sessions) != 1 {
			t.Fatal("未读取到合成会话所在页", index+1, err)
		}
		current := page.Sessions[0]
		if current.ObjectKey != key || current.InitiatedAt.IsZero() || current.InitiatedAt.Location() != time.UTC || current.InitiatedAt.Before(previousTime) {
			t.Fatal("存储返回的对象范围或发起时间不符", index+1)
		}
		previousTime = current.InitiatedAt
		gotIDs = append(gotIDs, current.UploadID)
		marker = page.NextUploadID
		if (index < len(wantIDs)-1 && marker != current.UploadID) || (index == len(wantIDs)-1 && marker != "") {
			t.Fatal("实际 MinIO 游标未推进或末页仍有标记", index+1)
		}
	}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatal("分页结果的原会话 ID 或顺序不符")
	}
	if page, err := client.ListMultipartUploadSessionsPage(ctx, emptyKey, "", 1); err != nil || page == nil || len(page.Sessions) != 0 || page.NextUploadID != "" {
		t.Fatal("另一个空对象键出现了会话", err)
	}

	// 3. 复查四个合成会话仍可按原始 ID 访问。
	for _, uploadID := range wantIDs {
		if _, err := client.ListUploadPartsPage(ctx, key, uploadID, 0, 1); err != nil {
			t.Fatal("分页后原会话无法访问", err)
		}
	}
	if _, err := client.ListUploadPartsPage(ctx, otherKey, otherID, 0, 1); err != nil {
		t.Fatal("另一账号的原会话无法访问", err)
	}
	t.Log("完整对象键连续读出三页，目录前缀返回空，四条合成会话均仍可读取")
}
