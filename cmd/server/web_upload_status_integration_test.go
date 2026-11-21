package main

import (
	"context"
	"slices"
	"testing"
	"time"
)

func TestServerWebSingleUploadStatusIntegration(t *testing.T) {
	cli := webFixtureBrowserCLI(t, "CLOUDNEST_WEB_UPLOAD_PLAYWRIGHT_CLI")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 在独占数据库准备稳定的任务状态，存储端只允许检查测试桶。
	startIsolatedWebMySQL(t, ctx)
	pool, webURL := prepareWebPurgePreviewFixture(t, ctx)
	for _, statement := range []string{
		`INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES
			(11,7,'28ee758a-0727-42de-ae7d-3bb33155e148','进度任务来源设备','WEB'),
			(12,8,'28ee758a-0727-42de-ae7d-3bb33155e149','其他合成账号来源设备','WEB')`,
		`INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size,status,verified_sha256,verified_at) VALUES
			(101,7,'MINIO','fixture-web-purge-preview','status-uploading',3,'STAGING',NULL,NULL),
			(102,7,'MINIO','fixture-web-purge-preview','status-verifying',6,'VERIFYING',NULL,NULL),
			(103,7,'MINIO','fixture-web-purge-preview','status-completed',8,'READY',UNHEX(REPEAT('23',32)),UTC_TIMESTAMP(6)),
			(104,7,'MINIO','fixture-web-purge-preview','status-failed',5,'ORPHANED',NULL,NULL),
			(105,8,'MINIO','fixture-web-purge-preview','status-other-user',2,'STAGING',NULL,NULL)`,
		`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size)
			VALUES(9007199254740995,7,33,'完成示例 (1).bin',CAST('完成示例 (1).bin' AS BINARY),'FILE','OTHER',103,8)`,
		`INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,published_object_id,target_parent_id,requested_name,resolved_name,
			file_size,upload_mode,status,result_entry_id,reserved_bytes,lease_version,next_attempt_at,expires_at,last_error_code,completed_at) VALUES
			(73,7,11,101,NULL,33,'超时复核.bin',NULL,3,'SINGLE','UPLOADING',NULL,3,0,NULL,UTC_TIMESTAMP(6)+INTERVAL 1 DAY,NULL,NULL),
			(74,7,11,102,NULL,33,'待校验.bin',NULL,6,'SINGLE','VERIFYING',NULL,6,1,UTC_TIMESTAMP(6)+INTERVAL 1 HOUR,UTC_TIMESTAMP(6)+INTERVAL 1 DAY,NULL,NULL),
			(9007199254740993,7,11,103,103,33,'完成示例.bin','完成示例 (1).bin',8,'SINGLE','COMPLETED',9007199254740995,0,2,NULL,UTC_TIMESTAMP(6)+INTERVAL 1 DAY,NULL,UTC_TIMESTAMP(6)),
			(76,7,11,104,NULL,33,'失败示例.bin',NULL,5,'SINGLE','FAILED',NULL,0,2,NULL,UTC_TIMESTAMP(6)+INTERVAL 1 DAY,'CHECKSUM_MISMATCH',UTC_TIMESTAMP(6)),
			(77,8,12,105,NULL,63,'其他账号任务.bin',NULL,2,'SINGLE','UPLOADING',NULL,2,0,NULL,UTC_TIMESTAMP(6)+INTERVAL 1 DAY,NULL,NULL)`,
		`UPDATE users SET used_storage=4104,reserved_storage=9 WHERE id=7`,
		`UPDATE users SET reserved_storage=2 WHERE id=8`,
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	snapshotTasks := func() string {
		t.Helper()
		var snapshot string
		if err := pool.QueryRowContext(ctx, `SELECT CAST(JSON_OBJECTAGG(CAST(id AS CHAR),JSON_ARRAY(status,reserved_bytes,
			result_entry_id,lease_version,attempt_count,next_attempt_at,expires_at,last_error_code,completed_at,updated_at)) AS CHAR)
			FROM upload_tasks`).Scan(&snapshot); err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	beforeTasks := snapshotTasks()
	beforeBusiness := readWebPurgePreviewSnapshot(t, ctx, pool)

	// 2. 浏览器用模拟回执打开预置任务，GET 走真实的登录、权限与读取链路。
	runWebBrowserProbe(t, ctx, cli, webURL, "web_upload_status.js", "CN525 browser checks passed")
	t.Log("Edge 验收通过：校验中、完成及大整数文件 ID、失败原因、超时复核、权限拒绝与错误重试")

	// 3. 核对只读查询期间任务、目录、配额和存储元数据没有变更。
	if beforeTasks != snapshotTasks() || !slices.Equal(beforeBusiness, readWebPurgePreviewSnapshot(t, ctx, pool)) {
		t.Fatal("只读状态查询改变了隔离库快照")
	}
	t.Log("数据库核对通过：预置的五个任务、目录项、对象、配额与幂等记录保持原样")
}
