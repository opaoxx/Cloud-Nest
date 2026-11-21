package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"slices"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
)

const webDownloadFixtureName = "隔离授权.bin"

func TestServerWebDownloadGrantIntegration(t *testing.T) {
	cli := webFixtureBrowserCLI(t, "CLOUDNEST_WEB_DOWNLOAD_PLAYWRIGHT_CLI")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 准备独占 MinIO 对象和目录。
	pool, webURL, _ := prepareWebDownloadFixture(t, ctx)
	before := readWebPurgePreviewSnapshot(t, ctx, pool)

	// 2. 通过目录页验收真实授权、旧版本重读、目标不可用和短期链接展示。
	runWebBrowserProbe(t, ctx, cli, webURL, "web_download_grant.js", "CN527 browser checks passed")
	t.Log("Edge 验收通过：旧版本 409 后读取列表，新版本 200 签发，缺少对象 409，浏览器未请求正文")

	// 3. 授权成功记录最近访问，目标文件、对象和使用空间保持原样。
	var records, validRecords int
	if err := pool.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(CASE WHEN user_id=7 AND entry_id=9007199254740993
		AND last_opened_at IS NOT NULL THEN 1 END) FROM user_entry_preferences`).Scan(&records, &validRecords); err != nil {
		t.Fatal(err)
	}
	if records != 1 || validRecords != 1 || !slices.Equal(before, readWebPurgePreviewSnapshot(t, ctx, pool)) {
		t.Fatal("授权的访问记录或文件数据不符")
	}
	t.Log("数据库核对通过：仅已授权文件产生最近访问记录，目录、对象和配额保持原样")
}

// prepareWebDownloadFixture 在独占库和桶创建实际可下载的合成文件及授权错误夹具。
func prepareWebDownloadFixture(t *testing.T, ctx context.Context) (*sql.DB, string, []byte) {
	t.Helper()
	pool, bucket, webURL := prepareWebSingleUploadFixture(t, ctx)
	payload := []byte{13, 42, 0, 255}
	digest := sha256.Sum256(payload)
	const objectKey = "download-fixture/source.bin"
	stored, err := bucket.client.PutObject(ctx, bucket.name, objectKey, bytes.NewReader(payload), int64(len(payload)), minio.PutObjectOptions{})
	if err != nil || stored.VersionID == "" {
		t.Fatal("无法准备联测对象版本", err)
	}
	if _, err := pool.ExecContext(ctx, `INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,storage_version_id,
		file_size,verified_sha256,status,verified_at) VALUES
		(41,7,'local_minio',?,?,?,4,?,'READY',UTC_TIMESTAMP(6)),
		(42,7,'local_minio',?,'download-fixture/missing.bin',NULL,4,UNHEX(REPEAT('23',32)),'READY',UTC_TIMESTAMP(6))`,
		bucket.name, objectKey, stored.VersionID, digest[:], bucket.name); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,revision) VALUES
		(9007199254740993,7,33,?,?,'FILE','OTHER',41,4,9007199254740995),
		(52,7,33,'缺少内容.bin',CAST('缺少内容.bin' AS BINARY),'FILE','OTHER',42,4,1)`, webDownloadFixtureName, []byte(webDownloadFixtureName)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type)
		VALUES(53,7,33,'隔离下载目录',CAST('隔离下载目录' AS BINARY),'DIR')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE users SET used_storage=8 WHERE id=7"); err != nil {
		t.Fatal(err)
	}
	return pool, webURL, payload
}
