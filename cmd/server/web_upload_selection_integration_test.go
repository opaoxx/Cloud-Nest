package main

import (
	"context"
	"slices"
	"testing"
	"time"
)

func TestServerWebSingleUploadSelectionIntegration(t *testing.T) {
	cli := webFixtureBrowserCLI(t, "CLOUDNEST_WEB_UPLOAD_PLAYWRIGHT_CLI")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 复用隔离服务和存储桩，为合成账号准备一个有已知 ID 的子目录。
	startIsolatedWebMySQL(t, ctx)
	pool, webURL := prepareWebPurgePreviewFixture(t, ctx)
	const childName = "隔离上传选择目录"
	if _, err := pool.ExecContext(ctx, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type)
		VALUES(54,7,33,?,?,'DIR')`, childName, []byte(childName)); err != nil {
		t.Fatal(err)
	}
	before := readWebPurgePreviewSnapshot(t, ctx, pool)

	// 2. 用合成文件验证两个目录的选择、边界错误及请求边界。
	runWebBrowserProbe(t, ctx, cli, webURL, "web_upload_selection.js", "CN522 browser checks passed")
	t.Log("Edge 验收通过：根目录和子目录、零字节和 16 MiB、超限和保留名、替换选择、无文件业务请求")

	// 3. 核对没有上传任务或预占写入。
	if after := readWebPurgePreviewSnapshot(t, ctx, pool); !slices.Equal(before, after) {
		t.Fatal("本地文件选择改变了数据库业务快照")
	}
	var uploads int
	if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM upload_tasks").Scan(&uploads); err != nil || uploads != 0 {
		t.Fatalf("隔离库的上传任务数应为 0，得到 %d，%v", uploads, err)
	}
	t.Log("数据库核对通过：上传任务为 0，目录项、对象、配额和幂等记录保持原样")
}
