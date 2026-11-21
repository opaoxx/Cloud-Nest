package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestServerWebDownloadSaveIntegration(t *testing.T) {
	cli := webFixtureBrowserCLI(t, "CLOUDNEST_WEB_DOWNLOAD_PLAYWRIGHT_CLI")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 使用独占库、MinIO 桶和合成文件，保持正式授权与浏览器下载链路。
	_, webURL, source := prepareWebDownloadFixture(t, ctx)

	// 2. 验证到期拦截和手动续签，再将浏览器附件保存在本轮临时目录。
	directory := runWebBrowserProbe(t, ctx, cli, webURL, "web_download_save.js", "CN528 browser checks passed")
	t.Log("Edge 验收通过：到期时没有正文 GET，续签后只用签名地址下载，保留中文附件名和文件页")

	// 3. 独立读回临时文件，核对字节数、内容和 SHA-256。
	actual, err := os.ReadFile(filepath.Join(directory, webDownloadFixtureName))
	if err != nil {
		t.Fatal("无法读回隔离浏览器保存的中文文件", err)
	}
	wantHash, actualHash := sha256.Sum256(source), sha256.Sum256(actual)
	if len(actual) != len(source) || !bytes.Equal(actual, source) || actualHash != wantHash {
		t.Fatalf("保存文件与合成源文件不符：字节数 %d，SHA-256 %x", len(actual), actualHash)
	}
	t.Logf("保存文件 %s，字节数 %d，源文件与保存结果的 SHA-256 均为 %x", webDownloadFixtureName, len(actual), actualHash)
}
