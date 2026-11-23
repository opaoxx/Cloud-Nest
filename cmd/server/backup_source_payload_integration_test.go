package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
)

func TestServerBackupSourcePayloadIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后保存隔离源端对象正文")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	fixture := prepareBackupSource(t, ctx)
	root, manifest := prepareBackupSourceBundle(t, ctx, fixture)

	// 1. 用清单的副本模拟异常，确认失败没有留下可用的正文文件。
	verifyBackupPayloadRejections(t, ctx, fixture.bucket, root, manifest.Objects[0])

	// 2. 按正确版本保存全部正文，再从磁盘读取并核对清单。
	saveAndVerifyBackupSourcePayloads(t, ctx, fixture.bucket, root, manifest.Objects)
}

func prepareBackupSourceBundle(t *testing.T, ctx context.Context, fixture backupSourceFixture) (*os.Root, backupSourceManifest) {
	t.Helper()
	// 在同一停写窗口准备 SQL 和清单，在它们旁边创建正文目录。
	verifyBackupSourceBaseline(t, ctx, fixture)
	backupDir := t.TempDir()
	dump := stopAndExportBackupSourceSQL(t, ctx, fixture, backupDir)
	manifest := writeAndVerifyBackupSourceManifest(t, ctx, fixture, backupDir, dump)
	return openBackupSourceBundleRoot(t, backupDir), manifest
}

func openBackupSourceBundleRoot(t *testing.T, backupDir string) *os.Root {
	t.Helper()
	// 打开备份目录并创建受根目录约束的正文子目录。
	root, err := os.OpenRoot(backupDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Error("关闭临时备份目录失败", err)
		}
	})
	if err := root.Mkdir("objects", 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func saveAndVerifyBackupSourcePayloads(t *testing.T, ctx context.Context, bucket *serverUploadContentBucket, root *os.Root, objects []backupManifestObject) {
	t.Helper()
	for _, object := range objects {
		if err := saveBackupObjectPayload(ctx, bucket, root, object); err != nil {
			t.Fatalf("保存对象 %d 的正文失败：%v", object.ID, err)
		}
	}
	verifySavedBackupPayloads(t, root, objects)
}

func saveBackupObjectPayload(ctx context.Context, source *serverUploadContentBucket, root *os.Root, object backupManifestObject) (returnErr error) {
	// 1. 按清单定位对象，限定源桶和备份目录内的文件名。
	if object.ID == 0 || object.Provider != "local_minio" || object.Bucket != source.name || object.Key == "" ||
		object.PayloadPath != fmt.Sprintf("objects/%d.bin", object.ID) || object.SizeBytes >= math.MaxInt64 {
		return errors.New("清单的对象定位、路径或大小无效")
	}
	if _, err := root.Lstat(object.PayloadPath); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return err
		}
		return errors.New("正文目标路径已存在")
	}
	options := minio.GetObjectOptions{}
	if object.VersionID != nil {
		if *object.VersionID == "" {
			return errors.New("对象版本为空字符串")
		}
		options.VersionID = *object.VersionID
	}
	options.Set("Accept-Encoding", "identity")
	body, info, headers, err := (minio.Core{Client: source.client}).GetObject(ctx, object.Bucket, object.Key, options)
	if err != nil {
		return err
	}
	defer body.Close()
	if headers.Get("Content-Range") != "" || (options.VersionID != "" && options.VersionID != info.VersionID &&
		!(options.VersionID == "null" && info.VersionID == "")) {
		return errors.New("MinIO 返回的对象版本或正文范围不符")
	}
	if info.Size < 0 || uint64(info.Size) != object.SizeBytes {
		return errors.New("MinIO 响应大小与清单不符")
	}

	// 2. 流式写入临时文件。最多多读一字节，以便发现正文超过声明大小。
	partialPath := object.PayloadPath + ".partial"
	file, err := root.OpenFile(partialPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if returnErr != nil {
			returnErr = errors.Join(returnErr, root.Remove(partialPath))
		}
	}()
	digest := sha256.New()
	copied, readErr := io.Copy(io.MultiWriter(file, digest), io.LimitReader(body, int64(object.SizeBytes)+1))
	if err := errors.Join(readErr, file.Close(), ctx.Err()); err != nil {
		return err
	}
	if uint64(copied) != object.SizeBytes || fmt.Sprintf("%x", digest.Sum(nil)) != object.SHA256 {
		return errors.New("正文实际字节数或 SHA-256 与清单不符")
	}

	// 3. 校验成功后使用清单指定的最终文件名。
	return root.Rename(partialPath, object.PayloadPath)
}

func verifyBackupPayloadRejections(t *testing.T, ctx context.Context, source *serverUploadContentBucket, root *os.Root, object backupManifestObject) {
	t.Helper()
	unknownVersion := uuid.NewString()
	wrongVersion, wrongSize, wrongDigest := object, object, object
	wrongVersion.VersionID = &unknownVersion
	wrongSize.SizeBytes++
	wrongDigest.SHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte("different backup payload")))
	for _, scenario := range []struct {
		name   string
		object backupManifestObject
		want   string
	}{
		{"不存在的对象版本", wrongVersion, "NoSuchVersion"},
		{"清单大小不符", wrongSize, "响应大小"},
		{"清单摘要不符", wrongDigest, "SHA-256"},
	} {
		err := saveBackupObjectPayload(ctx, source, root, scenario.object)
		if err == nil || (!strings.Contains(err.Error(), scenario.want) && minio.ToErrorResponse(err).Code != scenario.want) {
			t.Fatalf("%s 的错误不符：%v", scenario.name, err)
		}
		entries, readErr := fs.ReadDir(root.FS(), "objects")
		if readErr != nil || len(entries) != 0 {
			t.Fatalf("%s 后正文目录不为空：%v", scenario.name, readErr)
		}
	}
	t.Log("拒绝不存在的版本、错误大小及错误 SHA-256，正文目录保持为空")
}

func verifySavedBackupPayloads(t *testing.T, root *os.Root, objects []backupManifestObject) {
	t.Helper()
	entries, err := fs.ReadDir(root.FS(), "objects")
	if err != nil || len(entries) != len(objects) {
		t.Fatal("备份正文文件数与清单不符", err)
	}
	var total uint64
	for _, object := range objects {
		file, err := root.Open(object.PayloadPath)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.New()
		size, readErr := io.Copy(digest, file)
		if err := errors.Join(readErr, file.Close()); err != nil {
			t.Fatal(err)
		}
		sum := fmt.Sprintf("%x", digest.Sum(nil))
		if uint64(size) != object.SizeBytes || sum != object.SHA256 {
			t.Fatal("磁盘正文的字节数或摘要与清单不符", object.ID)
		}
		total += uint64(size)
		t.Logf("正文 %s，%d B，SHA-256 %s", object.PayloadPath, size, sum)
	}
	t.Logf("临时备份包含同一停写窗口的 SQL、JSON 和 %d 个正文文件，正文共 %d B", len(entries), total)
}
