package main

import (
	"context"
	"slices"
	"testing"
	"time"
)

func TestServerWebRasterPreviewIntegration(t *testing.T) {
	cli := webFixtureBrowserCLI(t, "CLOUDNEST_WEB_DOWNLOAD_PLAYWRIGHT_CLI")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 复用独占 MySQL、MinIO 和合成的 1×1 PNG。
	pool, webURL := prepareWebPreviewFixture(t, ctx)
	before := readWebPurgePreviewSnapshot(t, ctx, pool)

	// 2. 验证真实 PNG 读回和解码，以及到期和失败时的重新授权。
	runWebBrowserProbe(t, ctx, cli, webURL, "web_raster_preview.js", "CN530 browser checks passed")
	t.Log("Edge 验收通过：点击后才加载，真实 1×1 PNG 解码，模拟读取失败可重新获取，过期未发 GET，纯文本未加载")

	// 3. 核对显示图片只读存储内容，不改动目录、对象、配额或幂等记录。
	if !slices.Equal(before, readWebPurgePreviewSnapshot(t, ctx, pool)) {
		t.Fatal("隔离预览修改了文件数据")
	}
	t.Log("数据库核对通过：目录、对象、配额和幂等记录保持原样")
}
