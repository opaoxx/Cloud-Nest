package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/storage"
)

type backupTargetObjectMapping struct {
	ObjectID        uint64  `json:"object_id,string"`
	UserID          uint64  `json:"user_id,string"`
	Provider        string  `json:"provider"`
	ObjectKey       string  `json:"object_key"`
	SourceBucket    string  `json:"source_bucket"`
	SourceVersionID *string `json:"source_storage_version_id"`
	TargetBucket    string  `json:"target_bucket"`
	TargetVersionID string  `json:"target_storage_version_id"`
	TargetETag      string  `json:"target_etag"`
}

type backupTargetObjectMap struct {
	ManifestSHA256 string                      `json:"manifest_sha256"`
	Objects        []backupTargetObjectMapping `json:"objects"`
}

func TestServerBackupTargetObjectsIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后恢复隔离目标桶")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 从停写源端生成已校验的本机备份，再创建新的目标桶。
	source := prepareBackupSource(t, ctx)
	root, manifest := prepareBackupSourceBundle(t, ctx, source)
	saveAndVerifyBackupSourcePayloads(t, ctx, source.bucket, root, manifest.Objects)
	target, writer := prepareBackupTargetBucket(t, ctx, source)

	// 2. 模拟摘要损坏。失败必须发生在写入目标桶前。
	wrongDigest := manifest.Objects[0]
	wrongDigest.SHA256 = strings.Repeat("0", 64)
	if _, err := restoreBackupObject(ctx, writer, target, root, wrongDigest); err == nil || !strings.Contains(err.Error(), "本机备份正文") {
		t.Fatalf("备份校验失败未被拒绝：%v", err)
	}
	verifyBackupTargetVersions(t, ctx, target, nil)

	// 3. 恢复并读回全部版本，确认重复写入不产生额外版本，再保存映射。
	objects := make([]backupTargetObjectMapping, 0, len(manifest.Objects))
	for _, object := range manifest.Objects {
		mapping, err := restoreBackupObject(ctx, writer, target, root, object)
		if err != nil {
			t.Fatalf("恢复对象 %d 失败：%v", object.ID, err)
		}
		if object.VersionID == nil || mapping.TargetVersionID == *object.VersionID {
			t.Fatal("隔离目标桶应取得本次写入的新版本", object.ID)
		}
		objects = append(objects, mapping)
		t.Logf("对象 %d 键 %s：源桶 %s 版本 %s，目标桶 %s 版本 %s ETag %s，读回 %d B SHA-256 %s",
			mapping.ObjectID, mapping.ObjectKey, mapping.SourceBucket, *mapping.SourceVersionID,
			mapping.TargetBucket, mapping.TargetVersionID, mapping.TargetETag, object.SizeBytes, object.SHA256)
	}
	if _, err := restoreBackupObject(ctx, writer, target, root, manifest.Objects[0]); !errors.Is(err, domain.ErrUploadObjectExists) {
		t.Fatalf("重复对象键应被条件写入拒绝：%v", err)
	}
	verifyBackupTargetVersions(t, ctx, target, objects)
	writeAndVerifyBackupTargetObjectMap(t, root, objects)
	t.Log("拒绝摘要损坏和重复写入，目标桶恰好包含映射所列的三个版本")
}

func prepareBackupTargetBucket(t *testing.T, ctx context.Context, source backupSourceFixture) (*serverUploadContentBucket, *storage.Client) {
	t.Helper()
	sourceEndpoint := os.Getenv("CLOUDNEST_S3_ENDPOINT")
	containerID := startIsolatedWebMinIO(t, ctx)
	if targetEndpoint := os.Getenv("CLOUDNEST_S3_ENDPOINT"); containerID == source.minioContainer || targetEndpoint == sourceEndpoint {
		t.Fatal("目标 MinIO 必须使用独立容器和回环地址")
	}
	bucket := prepareServerUploadContentBucket(t, ctx)
	settings, err := config.LoadStorageConfig()
	if err != nil {
		t.Fatal(err)
	}
	if bucket.name == source.bucket.name || settings.Bucket != bucket.name {
		t.Fatal("恢复写入未限定在新建的目标桶")
	}
	writer, err := storage.NewClient(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(writer.Close)
	t.Logf("目标 MinIO 容器 %s，回环地址 %s，独占版本化桶 %s", containerID, settings.EndpointURL, bucket.name)
	return bucket, writer
}

func restoreBackupObject(ctx context.Context, writer *storage.Client, target *serverUploadContentBucket, root *os.Root, object backupManifestObject) (backupTargetObjectMapping, error) {
	// 1. 确认对象定位和本机备份摘要，再允许写入到目标桶。
	if object.ID == 0 || object.UserID == 0 || object.Provider != "local_minio" || object.Bucket == target.name || object.Key == "" ||
		object.PayloadPath != fmt.Sprintf("objects/%d.bin", object.ID) || object.SizeBytes > domain.MaxSingleUploadSizeBytes {
		return backupTargetObjectMapping{}, errors.New("待恢复对象的定位、路径或大小无效")
	}
	file, err := root.Open(object.PayloadPath)
	if err != nil {
		return backupTargetObjectMapping{}, err
	}
	defer file.Close()
	digest := sha256.New()
	size, err := io.Copy(digest, io.LimitReader(file, int64(object.SizeBytes)+1))
	if err != nil {
		return backupTargetObjectMapping{}, err
	}
	if uint64(size) != object.SizeBytes || fmt.Sprintf("%x", digest.Sum(nil)) != object.SHA256 {
		return backupTargetObjectMapping{}, errors.New("本机备份正文的大小或 SHA-256 不符")
	}

	// 2. 复用单文件条件写入，从开头提交原键并取得目标存储确认。
	stored, err := writer.PutUploadContent(ctx, object.Key, file, object.SizeBytes)
	if err != nil {
		return backupTargetObjectMapping{}, err
	}
	if stored == nil || stored.SizeBytes != object.SizeBytes || stored.StorageVersionID == nil || *stored.StorageVersionID == "" ||
		*stored.StorageVersionID == "null" || stored.StorageETag == nil || *stored.StorageETag == "" {
		return backupTargetObjectMapping{}, errors.New("目标桶写入回执缺少实际版本或 ETag")
	}

	// 3. 按新版本读取目标正文，比较版本、ETag、实际字节数和 SHA-256。
	version, etag := *stored.StorageVersionID, *stored.StorageETag
	options := minio.GetObjectOptions{VersionID: version}
	options.Set("Accept-Encoding", "identity")
	body, info, headers, err := (minio.Core{Client: target.client}).GetObject(ctx, target.name, object.Key, options)
	if err != nil {
		return backupTargetObjectMapping{}, err
	}
	digest.Reset()
	readBack, readErr := io.Copy(digest, io.LimitReader(body, int64(object.SizeBytes)+1))
	if err := errors.Join(readErr, body.Close(), ctx.Err()); err != nil {
		return backupTargetObjectMapping{}, err
	}
	if headers.Get("Content-Range") != "" || info.VersionID != version || info.ETag != etag || info.Size != int64(object.SizeBytes) ||
		uint64(readBack) != object.SizeBytes || fmt.Sprintf("%x", digest.Sum(nil)) != object.SHA256 {
		return backupTargetObjectMapping{}, errors.New("目标对象读回的版本、ETag、大小或 SHA-256 不符")
	}
	return backupTargetObjectMapping{ObjectID: object.ID, UserID: object.UserID, Provider: object.Provider, ObjectKey: object.Key,
		SourceBucket: object.Bucket, SourceVersionID: object.VersionID, TargetBucket: target.name, TargetVersionID: version, TargetETag: etag}, nil
}

func verifyBackupTargetVersions(t *testing.T, ctx context.Context, target *serverUploadContentBucket, objects []backupTargetObjectMapping) {
	t.Helper()
	expected := make(map[string]backupTargetObjectMapping, len(objects))
	for _, object := range objects {
		expected[object.ObjectKey] = object
	}
	for info := range target.client.ListObjects(ctx, target.name, minio.ListObjectsOptions{Recursive: true, WithVersions: true}) {
		object, ok := expected[info.Key]
		if info.Err != nil || !ok || info.IsDeleteMarker || info.VersionID != object.TargetVersionID || info.ETag != object.TargetETag {
			t.Fatal("目标桶有映射之外的对象或版本", info.Key, info.Err)
		}
		delete(expected, info.Key)
	}
	if len(expected) != 0 {
		t.Fatal("目标桶缺少映射中的对象版本")
	}
}

func writeAndVerifyBackupTargetObjectMap(t *testing.T, root *os.Root, objects []backupTargetObjectMapping) {
	t.Helper()
	manifest, err := root.ReadFile("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	mapping := backupTargetObjectMap{ManifestSHA256: fmt.Sprintf("%x", sha256.Sum256(manifest)), Objects: objects}
	encoded, err := json.MarshalIndent(mapping, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	const path = "object-version-map.json"
	if err := root.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	saved, err := root.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded backupTargetObjectMap
	if err := json.Unmarshal(saved, &decoded); err != nil || !reflect.DeepEqual(decoded, mapping) {
		t.Fatal("版本映射读回与写入结果不符", err)
	}
	t.Logf("映射 %d B，SHA-256 %x，绑定清单 SHA-256 %s", len(saved), sha256.Sum256(saved), decoded.ManifestSHA256)
}
