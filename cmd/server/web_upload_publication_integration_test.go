package main

import (
	"context"
	"testing"
	"time"
)

func TestServerWebSingleUploadPublicationIntegration(t *testing.T) {
	cli := webFixtureBrowserCLI(t, "CLOUDNEST_WEB_UPLOAD_PLAYWRIGHT_CLI")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 在独占库建立目标目录和占用原名的目录项，保留冲突编号给真实发布处理。
	pool, bucket, webURL := prepareWebSingleUploadFixture(t, ctx)
	if _, err := pool.ExecContext(ctx, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES
		(54,7,33,'隔离发布目录',CAST('隔离发布目录' AS BINARY),'DIR'),
		(55,7,54,'重名资料.bin',CAST('重名资料.bin' AS BINARY),'DIR')`); err != nil {
		t.Fatal(err)
	}

	// 2. 通过真实 Web 上传。观察详情读失败后重试，再刷新原目录。
	runWebBrowserProbe(t, ctx, cli, webURL, "web_upload_publication.js", "CN526 browser checks passed")
	t.Log("Edge 验收通过：完成后按结果 ID 查询、详情失败可重试、目录只在读回文件后刷新")

	// 3. 从隔离库确认列表中的文件正是实际发布的目录项。
	var verified int
	if err := pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM upload_tasks t
		JOIN file_entries f ON f.id=t.result_entry_id AND f.user_id=t.user_id
		JOIN stored_objects o ON o.id=f.object_id AND o.owner_user_id=f.user_id
		JOIN users u ON u.id=t.user_id
		WHERE t.user_id=7 AND t.target_parent_id=54 AND t.requested_name='重名资料.bin' AND t.status='COMPLETED'
		AND t.resolved_name='重名资料 (1).bin' AND t.published_object_id=o.id AND t.reserved_bytes=0
		AND f.parent_id=54 AND f.name=t.resolved_name AND f.entry_type='FILE' AND f.status='ACTIVE' AND f.file_size=4
		AND o.bucket=? AND o.status='READY' AND u.used_storage=4 AND u.reserved_storage=0`, bucket.name).Scan(&verified); err != nil {
		t.Fatal(err)
	}
	if verified != 1 {
		t.Fatal("隔离库缺少与任务回执对应的实际发布文件")
	}
	t.Log("数据库核对通过：任务 COMPLETED，真实目录项重名资料 (1).bin，4 字节预占转为已用")
}
