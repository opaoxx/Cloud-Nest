package main

import (
	"context"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
)

func TestServerWebSingleUploadCancelIntegration(t *testing.T) {
	cli := webFixtureBrowserCLI(t, "CLOUDNEST_WEB_UPLOAD_PLAYWRIGHT_CLI")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 在专用库准备一个有有效租约的任务和一个已过期任务。
	pool, bucket, webURL := prepareWebSingleUploadFixture(t, ctx)
	if _, err := pool.ExecContext(ctx, `INSERT INTO user_devices(id,user_id,installation_id,name,platform)
		VALUES(11,7,'28ee758a-0727-42de-ae7d-3bb33155e148','取消夹具来源设备','WEB')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, `INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size,status,unreferenced_at,gc_after)
		VALUES(101,7,'MINIO',?,'cancel-fixture-busy',3,'STAGING',NULL,NULL),
		(102,7,'MINIO',?,'cancel-fixture-expired',3,'ORPHANED',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6)+INTERVAL 7 DAY)`, bucket.name, bucket.name); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, `INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,
		file_size,upload_mode,status,reserved_bytes,lease_owner,lease_until,lease_version,expires_at,completed_at) VALUES
		(81,7,11,101,33,'隔离占用.bin',3,'SINGLE','UPLOADING',3,'fixture-active-receiver',UTC_TIMESTAMP(6)+INTERVAL 1 HOUR,1,UTC_TIMESTAMP(6)+INTERVAL 1 DAY,NULL),
		(82,7,11,102,33,'隔离过期.bin',3,'SINGLE','EXPIRED',0,NULL,NULL,1,UTC_TIMESTAMP(6)-INTERVAL 1 HOUR,UTC_TIMESTAMP(6))`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, `UPDATE users SET reserved_storage=3 WHERE id=7`); err != nil {
		t.Fatal(err)
	}

	// 2. 观察浏览器请求。只有显式确认的取消才允许发往隔离后端。
	runWebBrowserProbe(t, ctx, cli, webURL, "web_upload_cancel.js", "CN532 browser checks passed")
	t.Log("Edge 验收通过：退出确认零写入，连点只提交一次，真实 202、409、200，回执异常先查询再确认")

	// 3. 核对两个对照任务未改变，受理任务未发布文件或写入对象正文。
	var tasks, unchanged, accepted, files, accounted int
	if err := pool.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM upload_tasks),
		(SELECT COUNT(*) FROM upload_tasks WHERE (id=81 AND status='UPLOADING' AND reserved_bytes=3 AND lease_owner='fixture-active-receiver')
			OR (id=82 AND status='EXPIRED' AND reserved_bytes=0)),
		(SELECT COUNT(*) FROM upload_tasks WHERE user_id=7 AND requested_name='隔离取消.bin' AND result_entry_id IS NULL
			AND ((status='CANCELLING' AND reserved_bytes=3) OR (status='CANCELLED' AND reserved_bytes=0))),
		(SELECT COUNT(*) FROM file_entries WHERE entry_type='FILE'),
		(SELECT COUNT(*) FROM users WHERE id=7 AND used_storage=0
			AND reserved_storage=(SELECT SUM(reserved_bytes) FROM upload_tasks WHERE user_id=7))`).Scan(&tasks, &unchanged, &accepted, &files, &accounted); err != nil {
		t.Fatal(err)
	}
	if tasks != 3 || unchanged != 2 || accepted != 1 || files != 0 || accounted != 1 {
		t.Fatalf("隔离库状态不符，任务=%d 对照=%d 已受理=%d 已发布文件=%d 配额核对=%d", tasks, unchanged, accepted, files, accounted)
	}
	for object := range bucket.client.ListObjects(ctx, bucket.name, minio.ListObjectsOptions{Recursive: true, WithVersions: true}) {
		if object.Err != nil {
			t.Fatal(object.Err)
		}
		t.Fatal("隔离桶不应存在对象版本")
	}
	t.Log("MySQL/MinIO 核对通过：两条对照任务保持原状，取消任务未发布，已用空间为零，隔离桶没有对象版本")
}
