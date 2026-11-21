package main

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestServerWebSingleUploadCreationIntegration(t *testing.T) {
	cli := webFixtureBrowserCLI(t, "CLOUDNEST_WEB_UPLOAD_PLAYWRIGHT_CLI")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 只使用独占库和只读存储桩。合成账号已有 4096 字节用量，另留 8 字节可预占。
	startIsolatedWebMySQL(t, ctx)
	pool, webURL := prepareWebPurgePreviewFixture(t, ctx)
	if _, err := pool.ExecContext(ctx, "UPDATE users SET storage_quota=4104 WHERE id=7"); err != nil {
		t.Fatal(err)
	}
	const childName = "隔离任务目标目录"
	if _, err := pool.ExecContext(ctx, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type)
		VALUES(54,7,33,?,?,'DIR')`, childName, []byte(childName)); err != nil {
		t.Fatal(err)
	}
	entriesOnly := func(snapshot []string) []string {
		return slices.DeleteFunc(snapshot, func(row string) bool { return !strings.HasPrefix(row, `["entry",`) })
	}
	before := entriesOnly(readWebPurgePreviewSnapshot(t, ctx, pool))

	// 2. 用合成文件验收单次创建、人工同键重试及配额拒绝。
	runWebBrowserProbe(t, ctx, cli, webURL, "web_upload_creation.js", "CN523 browser checks passed")
	t.Log("Edge 验收通过：显式创建、连点仅发一次、回执丢失及无效路径、同键恢复 201、新键请求 507、零内容请求")

	// 3. 核对只预占一次。任务停在 UPLOADING，暂存对象只是数据库中的 STAGING 记录。
	var uploads, validTask, stagedObjects, idempotencyRecords int
	if err := pool.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM upload_tasks),
		(SELECT COUNT(*) FROM upload_tasks t JOIN users u ON u.id=t.user_id
		 JOIN stored_objects o ON o.id=t.staged_object_id AND o.owner_user_id=u.id
		 JOIN idempotency_records r ON r.user_id=u.id AND r.resource_id=t.id AND r.operation=?
		 WHERE u.id=7 AND t.target_parent_id=54 AND t.requested_name='任务资料.MD' AND t.file_size=5
		 AND t.upload_mode='SINGLE' AND t.status='UPLOADING' AND t.reserved_bytes=5 AND t.lease_version=0
		 AND t.result_entry_id IS NULL AND t.published_object_id IS NULL
		 AND o.status='STAGING' AND u.used_storage=4096 AND u.reserved_storage=5
		 AND r.resource_type='UPLOAD_TASK' AND r.response_status=201),
		(SELECT COUNT(*) FROM stored_objects WHERE status='STAGING'),
		(SELECT COUNT(*) FROM idempotency_records)`, domain.CreateUploadOperation).
		Scan(&uploads, &validTask, &stagedObjects, &idempotencyRecords); err != nil {
		t.Fatal(err)
	}
	if uploads != 1 || validTask != 1 || stagedObjects != 1 || idempotencyRecords != 1 {
		t.Fatalf("创建及重试应只预占一次，任务=%d 合法快照=%d 暂存对象=%d 幂等记录=%d", uploads, validTask, stagedObjects, idempotencyRecords)
	}
	if after := entriesOnly(readWebPurgePreviewSnapshot(t, ctx, pool)); !slices.Equal(before, after) {
		t.Fatal("上传任务创建不应发布或改变目录项")
	}
	t.Log("数据库核对通过：一个 SINGLE/UPLOADING 任务，一个 STAGING 记录，预占 5 字节，已用 4096 字节，目录项保持原样")
}
