package storage_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
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

func TestPutUploadContentMinIOIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_UPLOAD_STORAGE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_UPLOAD_STORAGE_TEST=1 后在已有桶的独立前缀内验证写入及回收")
	}
	settings, err := config.LoadStorageConfig()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := storage.NewClient(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	// 1. 使用独立 SDK 核对实际对象，所有测试对象归属本次随机前缀。
	transport, err := minio.DefaultTransport(settings.EndpointURL.Scheme == "https")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(transport.CloseIdleConnections)
	verifier, err := minio.New(settings.EndpointURL.Host, &minio.Options{
		Creds: credentials.NewStaticV4(settings.AccessKey, settings.SecretKey, ""), Secure: settings.EndpointURL.Scheme == "https",
		Transport: transport, BucketLookup: minio.BucketLookupPath, MaxRetries: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	identifier, err := uuid.NewRandom()
	if err != nil {
		t.Fatal(err)
	}
	prefix := "cloudnest-upload-verify/" + identifier.String() + "/"
	t.Cleanup(func() {
		cleanupContext, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		options := minio.ListObjectsOptions{Prefix: prefix, Recursive: true, WithVersions: true}
		for object := range verifier.ListObjects(cleanupContext, settings.Bucket, options) {
			if object.Err != nil {
				t.Error("列举本次测试对象版本失败")
				return
			}
			if !strings.HasPrefix(object.Key, prefix) || object.Key == prefix {
				t.Error("拒绝清理本次前缀范围外的对象")
				return
			}
			if err := verifier.RemoveObject(cleanupContext, settings.Bucket, object.Key, minio.RemoveObjectOptions{VersionID: object.VersionID}); err != nil {
				t.Error("删除本次测试对象版本失败")
			}
		}
		for object := range verifier.ListObjects(cleanupContext, settings.Bucket, options) {
			if object.Err != nil {
				t.Error("复核本次测试前缀失败")
			} else {
				t.Error("本次测试前缀仍有对象版本")
			}
		}
	})
	// 2. 目录初始化和已有文件服务共同提供经过校验的稳定临时文件。
	tempSettings := config.UploadTempConfig{Directory: filepath.Join(t.TempDir(), "uploads")}
	if err := storage.PrepareUploadTempDirectory(ctx, tempSettings); err != nil {
		t.Fatal(err)
	}
	space := service.NewUploadTempSpace()
	receiver, err := service.NewUploadContentFileService(tempSettings.Directory, space)
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		name    string
		payload []byte
	}{
		{"empty", nil},
		{"binary", []byte{0, 0xff, 0xfe, 1, 0x80}},
		{"SINGLE-limit", bytes.Repeat([]byte{0xab}, int(domain.MaxSingleUploadSizeBytes))},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			key := prefix + fixture.name
			expected := sha256.Sum256(fixture.payload)
			var result *domain.UploadObjectResult
			err := receiver.WithFile(ctx, bytes.NewReader(fixture.payload), uint64(len(fixture.payload)), &expected, func(content io.ReadSeeker, digest domain.UploadContentDigest) error {
				var err error
				result, err = client.PutUploadContent(ctx, key, content, digest.SizeBytes)
				return err
			})
			if err != nil || result == nil || result.SizeBytes != uint64(len(fixture.payload)) || result.StorageETag == nil || *result.StorageETag == "" {
				t.Fatal("真实暂存对象写入失败", err)
			}
			// 3. 同键重写被条件创建拒绝，随后读回原始对象核对完整内容。
			replacement := []byte("replacement")
			if repeated, err := client.PutUploadContent(ctx, key, bytes.NewReader(replacement), uint64(len(replacement))); !errors.Is(err, domain.ErrUploadObjectExists) || repeated != nil {
				t.Fatal("已有暂存对象未被条件写入保护", err)
			}
			object, err := verifier.GetObject(ctx, settings.Bucket, key, minio.GetObjectOptions{})
			if err != nil {
				t.Fatal(err)
			}
			info, statError := object.Stat()
			payload, readError := io.ReadAll(object)
			closeError := object.Close()
			if statError != nil || readError != nil || closeError != nil || !bytes.Equal(payload, fixture.payload) || sha256.Sum256(payload) != expected || info.ETag != *result.StorageETag || info.Size != int64(len(fixture.payload)) {
				t.Fatal("真实对象内容、大小或 ETag 与确认结果不符", statError, readError, closeError)
			}
			if (result.StorageVersionID == nil) != (info.VersionID == "") || (result.StorageVersionID != nil && *result.StorageVersionID != info.VersionID) {
				t.Fatal("真实对象版本定位不符")
			}
			entries, err := os.ReadDir(tempSettings.Directory)
			if err != nil || len(entries) != 0 {
				t.Fatal("存储写入后本地文件未回收", err)
			}
		})
	}
	release, err := space.Reserve(ctx, 128*1024*1024)
	if err != nil {
		t.Fatal("真实存储写入结束后字节预算未释放", err)
	}
	release()
}
