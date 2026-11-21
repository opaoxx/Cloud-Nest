package main

import (
	"bytes"
	"context"
	"slices"
	"testing"
	"time"
)

func TestServerWebPlainTextPreviewIntegration(t *testing.T) {
	cli := webFixtureBrowserCLI(t, "CLOUDNEST_WEB_DOWNLOAD_PLAYWRIGHT_CLI")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 复用独占数据库和 MinIO，增加真实的超限文本和零字节文本。
	const textLimit = 2 * 1024 * 1024
	pool, webURL := prepareWebPreviewFixture(t, ctx,
		webPreviewFixtureEntry{67, "超限预览.txt", "DOCUMENT", "text/plain", 1, bytes.Repeat([]byte{'a'}, textLimit+1)},
		webPreviewFixtureEntry{68, "空预览.txt", "DOCUMENT", "text/plain", 1, nil},
	)
	before := readWebPurgePreviewSnapshot(t, ctx, pool)

	// 2. 用真实签名和正文验证 UTF-8 显示、超限拒绝及错误恢复。
	runWebBrowserProbe(t, ctx, cli, webURL, "web_plain_text_preview.js", "CN531 browser checks passed")
	t.Log("Edge 验收通过：带标签的 UTF-8 正文按文本显示，零字节显示为空，超限拒绝，过期未发 GET，读取失败可恢复")

	// 3. 核对预览正文读取没有修改文件、对象、配额或幂等数据。
	if !slices.Equal(before, readWebPurgePreviewSnapshot(t, ctx, pool)) {
		t.Fatal("纯文本预览修改了隔离文件数据")
	}
	t.Log("数据库核对通过：目录、对象、配额和幂等记录保持原样")
}
