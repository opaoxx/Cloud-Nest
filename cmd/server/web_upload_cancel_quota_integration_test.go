package main

import (
	"context"
	"testing"
	"time"
)

func TestServerWebSingleUploadCancelQuotaIntegration(t *testing.T) {
	cli := webFixtureBrowserCLI(t, "CLOUDNEST_WEB_UPLOAD_PLAYWRIGHT_CLI")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 预置两个尚未写入正文的任务。取消目标预占 3 字节，对照任务预占 2 字节。
	pool, bucket, webURL := prepareWebSingleUploadFixture(t, ctx)
	if _, err := pool.ExecContext(ctx, `INSERT INTO user_devices(id,user_id,installation_id,name,platform)
		VALUES(11,7,'28ee758a-0727-42de-ae7d-3bb33155e148','配额夹具来源设备','WEB')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, `INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size,status)
		VALUES(101,7,'MINIO',?,'cancel-quota-target',3,'STAGING'),(102,7,'MINIO',?,'cancel-quota-control',2,'STAGING')`,
		bucket.name, bucket.name); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, `INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,
		file_size,upload_mode,status,reserved_bytes,expires_at) VALUES
		(81,7,11,101,33,'隔离配额取消.bin',3,'SINGLE','UPLOADING',3,UTC_TIMESTAMP(6)+INTERVAL 1 DAY),
		(82,7,11,102,33,'隔离配额对照.bin',2,'SINGLE','UPLOADING',2,UTC_TIMESTAMP(6)+INTERVAL 1 DAY)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, `UPDATE users SET reserved_storage=5 WHERE id=7`); err != nil {
		t.Fatal(err)
	}

	// 2. 使用真实取消接口和后台收尾，再验证终态查询后读取本人资料。
	runWebBrowserProbe(t, ctx, cli, webURL, "web_upload_cancel_quota.js", "CN533 browser checks passed")
	t.Log("Edge 验收通过：202 未触发配额读取，真实 CANCELLED 后读取失败保留终态，手动重查使预占从 5 B 降到 2 B")

	// 3. 核对后台只释放了目标任务的 3 字节，未改变对照任务或发布文件。
	var cancelled, untouched, accounted, files int
	if err := pool.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM upload_tasks t JOIN stored_objects o ON o.id=t.staged_object_id AND o.owner_user_id=t.user_id
			WHERE t.id=81 AND t.status='CANCELLED' AND t.reserved_bytes=0 AND t.result_entry_id IS NULL
			AND t.completed_at IS NOT NULL AND o.status='ORPHANED' AND o.gc_after=o.unreferenced_at+INTERVAL 7 DAY),
		(SELECT COUNT(*) FROM upload_tasks t JOIN stored_objects o ON o.id=t.staged_object_id AND o.owner_user_id=t.user_id
			WHERE t.id=82 AND t.status='UPLOADING' AND t.reserved_bytes=2 AND t.completed_at IS NULL AND o.status='STAGING'),
		(SELECT COUNT(*) FROM users WHERE id=7 AND reserved_storage=2 AND used_storage=0),
		(SELECT COUNT(*) FROM file_entries WHERE entry_type='FILE')`).Scan(&cancelled, &untouched, &accounted, &files); err != nil {
		t.Fatal(err)
	}
	if cancelled != 1 || untouched != 1 || accounted != 1 || files != 0 {
		t.Fatalf("隔离库状态不符，已取消=%d 对照=%d 配额=%d 发布文件=%d", cancelled, untouched, accounted, files)
	}
	t.Log("MySQL 核对通过：目标任务释放 3 字节，对照任务保留 2 字节，已用 0 字节，暂存对象进入 7 天回收等待")
}
