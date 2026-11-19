package storage_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/internal/server/storage"
)

func TestOpenUploadVerificationObjectMinIOIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_UPLOAD_STORAGE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_UPLOAD_STORAGE_TEST=1 后创建独占版本桶验证对象读取")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	settings, err := config.LoadStorageConfig()
	if err != nil {
		t.Fatal(err)
	}
	secure := settings.EndpointURL.Scheme == "https"
	transport, err := minio.DefaultTransport(secure)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(transport.CloseIdleConnections)
	verifier, err := minio.New(settings.EndpointURL.Host, &minio.Options{
		Creds: credentials.NewStaticV4(settings.AccessKey, settings.SecretKey, ""), Secure: secure,
		Transport: transport, BucketLookup: minio.BucketLookupPath, MaxRetries: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	const bucketPrefix = "cloudnest-version-it-"
	bucket := bucketPrefix + uuid.NewString()
	if err := verifier.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatal("创建独占版本桶失败", minio.ToErrorResponse(err).Code)
	}
	t.Cleanup(func() {
		if _, err := uuid.Parse(strings.TrimPrefix(bucket, bucketPrefix)); !strings.HasPrefix(bucket, bucketPrefix) || err != nil {
			t.Error("拒绝清理非本轮独占桶")
			return
		}
		cleanupContext, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		for object := range verifier.ListObjects(cleanupContext, bucket, minio.ListObjectsOptions{Recursive: true, WithVersions: true}) {
			if object.Err != nil || verifier.RemoveObject(cleanupContext, bucket, object.Key, minio.RemoveObjectOptions{VersionID: object.VersionID}) != nil {
				t.Error("独占桶对象版本清理失败")
				return
			}
		}
		if err := verifier.RemoveBucket(cleanupContext, bucket); err != nil {
			t.Error("独占版本桶清理失败")
			return
		}
		if exists, err := verifier.BucketExists(cleanupContext, bucket); err != nil || exists {
			t.Error("独占版本桶清理复核失败")
		}
	})
	settings.Bucket = bucket
	client, err := storage.NewClient(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// 1. 生成初始 null 版本和两个不同的显式版本。
	const key = "users/7/objects/version-fixture"
	put := func(content []byte) minio.UploadInfo {
		t.Helper()
		info, err := verifier.PutObject(ctx, bucket, key, bytes.NewReader(content), int64(len(content)), minio.PutObjectOptions{
			ContentType: "application/octet-stream", DisableMultipart: true,
		})
		if err != nil {
			t.Fatal("写入联测对象失败", minio.ToErrorResponse(err).Code)
		}
		return info
	}
	initial := put([]byte("before versioning"))
	if err := verifier.EnableVersioning(ctx, bucket); err != nil {
		t.Fatal("无法启用独占桶的版本功能")
	}
	oldBytes := []byte{0, 0xff, 0xfe, 0x80}
	old := put(oldBytes)
	latest := put(nil)
	if old.VersionID == "" || old.VersionID == "null" || latest.VersionID == "" || latest.VersionID == old.VersionID {
		t.Fatal("未创建两个独立版本")
	}

	// 2. 从同一个键打开精确版本，并独立比较原始字节。
	object := domain.StoredObject{ID: 23, OwnerUserID: 7, Provider: settings.Provider, Bucket: bucket, ObjectKey: key, Status: domain.StoredObjectStatusVerifying}
	for _, fixture := range []struct {
		name          string
		version, etag *string
		data          []byte
	}{
		{"original null version", new("null"), &initial.ETag, []byte("before versioning")},
		{"old explicit version", &old.VersionID, &old.ETag, oldBytes},
		{"latest with no version", nil, &latest.ETag, nil},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			record := object
			record.StorageVersionID, record.StorageETag = fixture.version, fixture.etag
			stream, err := client.OpenUploadVerificationObject(ctx, &record)
			if err != nil {
				t.Fatal(err)
			}
			data, readError := io.ReadAll(io.LimitReader(stream, int64(len(fixture.data))+1))
			closeError := stream.Close()
			if readError != nil || closeError != nil || !bytes.Equal(data, fixture.data) {
				t.Fatal("读回内容与指定版本不符", readError, closeError)
			}
		})
	}
	missing := uuid.NewString()
	object.StorageVersionID = &missing
	if stream, err := client.OpenUploadVerificationObject(ctx, &object); stream != nil || !errors.Is(err, domain.ErrUploadObjectMissing) {
		t.Fatal("未知版本未按缺失交付", stream, err)
	}

	// 3. 将同一历史版本交给实际校验摘要方法，核对精确大小与两侧边界。
	object.StorageVersionID, object.StorageETag = &old.VersionID, &old.ETag
	for _, fixture := range []struct {
		name     string
		declared uint64
		wantErr  error
	}{
		{"digest matches", uint64(len(oldBytes)), nil},
		{"digest rejects short object", uint64(len(oldBytes) + 1), domain.ErrUploadBodySizeMismatch},
		{"digest rejects long object", uint64(len(oldBytes) - 1), domain.ErrUploadBodySizeMismatch},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			stream, err := client.OpenUploadVerificationObject(ctx, &object)
			if err != nil {
				t.Fatal(err)
			}
			digest, readError := service.ReadUploadVerificationDigest(ctx, stream, fixture.declared)
			closeError := stream.Close()
			if !errors.Is(readError, fixture.wantErr) || closeError != nil {
				t.Fatal("历史版本大小校验或关闭失败", readError, closeError)
			}
			if fixture.wantErr == nil && (digest == nil || digest.SizeBytes != fixture.declared || digest.SHA256 != sha256.Sum256(oldBytes)) {
				t.Fatal("历史版本大小或摘要错误", digest)
			}
			if fixture.wantErr != nil && digest != nil {
				t.Fatal("大小不符却返回了摘要", digest)
			}
		})
	}

	// 4. 用历史版本完成打开、摘要比对与关闭的完整校验调用。
	t.Run("verify old object", func(t *testing.T) {
		record := object
		record.FileSize = uint64(len(oldBytes))
		now := time.Now().UTC()
		expected := sha256.Sum256(oldBytes)
		task := &domain.UploadTask{
			ID: 19, UserID: record.OwnerUserID, StagedObjectID: record.ID, UploadMode: "SINGLE", Status: domain.UploadStatusVerifying,
			FileSize: record.FileSize, ExpectedSHA256: &expected,
			LeaseVersion: 1, LeaseOwner: new("fixture-worker"), UpdatedAt: now, LeaseUntil: new(now.Add(time.Minute)),
		}
		result, err := service.VerifyUploadObject(ctx, task, &record, client.OpenUploadVerificationObject)
		if err != nil || result == nil || result.SizeBytes != record.FileSize || result.SHA256 != expected {
			t.Fatal("历史版本校验结果错误", result, err)
		}
		task.ExpectedSHA256 = new(sha256.Sum256([]byte("another object")))
		result, err = service.VerifyUploadObject(ctx, task, &record, client.OpenUploadVerificationObject)
		if result != nil || !errors.Is(err, domain.ErrUploadChecksumMismatch) {
			t.Fatal("历史版本摘要不匹配未拒绝", result, err)
		}
	})
}
