package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"image"
	"image/png"
	"slices"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
)

func TestServerWebPreviewGrantIntegration(t *testing.T) {
	cli := webFixtureBrowserCLI(t, "CLOUDNEST_WEB_DOWNLOAD_PLAYWRIGHT_CLI")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 在独占存储准备实际 PNG、纯文本及没有已确认 MIME 的文件。
	pool, bucket, webURL := prepareWebSingleUploadFixture(t, ctx)
	var raster bytes.Buffer
	if err := png.Encode(&raster, image.NewNRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	usedBytes := 0
	for _, fixture := range []struct {
		id       uint64
		name     string
		category string
		mimeType any
		revision uint64
		body     []byte
	}{
		{64, "合成预览.png", "IMAGE", "image/png", 9007199254740995, raster.Bytes()},
		{65, "合成预览.txt", "DOCUMENT", "text/plain", 2, []byte("隔离纯文本预览。")},
		{66, "不支持预览.bin", "OTHER", nil, 1, []byte{1, 4, 2}},
	} {
		key := fmt.Sprintf("preview-fixture/%d", fixture.id)
		stored, err := bucket.client.PutObject(ctx, bucket.name, key, bytes.NewReader(fixture.body), int64(len(fixture.body)), minio.PutObjectOptions{})
		if err != nil || stored.VersionID == "" {
			t.Fatal("无法准备隔离预览对象", err)
		}
		digest := sha256.Sum256(fixture.body)
		if _, err := pool.ExecContext(ctx, `INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,storage_version_id,
			file_size,verified_sha256,status,verified_at) VALUES(?,7,'local_minio',?,?,?,?,?,'READY',UTC_TIMESTAMP(6))`,
			fixture.id, bucket.name, key, stored.VersionID, len(fixture.body), digest[:]); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,
			object_id,file_size,revision,mime_type) VALUES(?,7,33,?,?,'FILE',?,?,?,?,?)`,
			fixture.id, fixture.name, []byte(fixture.name), fixture.category, fixture.id, len(fixture.body), fixture.revision, fixture.mimeType); err != nil {
			t.Fatal(err)
		}
		usedBytes += len(fixture.body)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE users SET used_storage=? WHERE id=7", usedBytes); err != nil {
		t.Fatal(err)
	}
	before := readWebPurgePreviewSnapshot(t, ctx, pool)

	// 2. 通过实际签名入口验收预览用途、响应检查和手动切换下载。
	runWebBrowserProbe(t, ctx, cli, webURL, "web_preview_grant.js", "CN529 browser checks passed")
	t.Log("Edge 验收通过：PNG 与纯文本 200 inline，无效媒体类型被拒，不支持格式 422 后手动申请附件链接，零正文请求")

	// 3. 核对授权成功产生的访问记录和文件数据边界。
	var recordCount int
	if err := pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_entry_preferences
		WHERE user_id=7 AND entry_id IN (64,65,66) AND last_opened_at IS NOT NULL`).Scan(&recordCount); err != nil {
		t.Fatal(err)
	}
	if recordCount != 3 || !slices.Equal(before, readWebPurgePreviewSnapshot(t, ctx, pool)) {
		t.Fatal("隔离预览授权后的访问记录或文件数据不符")
	}
	t.Log("数据库核对通过：三个授权文件均有最近访问记录，目录、对象和配额保持原样")
}
