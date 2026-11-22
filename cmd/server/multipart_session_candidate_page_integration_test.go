package main

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/internal/server/storage"
)

func TestServerMultipartSessionCandidatePageIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MULTIPART_SESSION_CANDIDATE_PAGE_TEST") != "1" {
		t.Skip("显式启用后创建专用 MySQL、MinIO 和合成会话")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 75*time.Second)
	defer cancel()
	mysqlID := startIsolatedWebMySQL(t, ctx)
	t.Logf("专用 MySQL %s，回环地址 %s", mysqlID[:12], os.Getenv("CLOUDNEST_DB_ADDRESS"))
	minioID := startIsolatedWebMinIO(t, ctx)
	t.Logf("专用 MinIO %s，回环地址 %s", minioID[:12], os.Getenv("CLOUDNEST_S3_ENDPOINT"))
	bucket := prepareServerUploadContentBucket(t, ctx)
	pool, _ := prepareServerIntegrationDatabase(t, ctx)
	pool.SetMaxOpenConns(1)
	store, err := repository.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := config.LoadStorageConfig()
	if err != nil {
		t.Fatal(err)
	}
	client, err := storage.NewClient(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	key, err := service.GenerateUploadObjectKey(7)
	if err != nil {
		t.Fatal(err)
	}
	core := minio.Core{Client: bucket.client}

	// 1. 同一键下创建三条会话，只有第一条写为任务当前绑定。
	var sessions []string
	for range 3 {
		uploadID, err := client.CreateMultipartUpload(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, uploadID)
		t.Cleanup(func() {
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			if err := core.AbortMultipartUpload(cleanup, bucket.name, key, uploadID); err != nil {
				t.Error("合成会话清理失败", minio.ToErrorResponse(err).Code)
			}
		})
	}
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	const partSize, fileSize = 16 << 20, (16 << 20) + 1
	clock := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	execute("SET timestamp = ?", clock.Unix())
	defer execute("SET timestamp = DEFAULT")
	execute("INSERT INTO users(id,username,display_name,password_hash,used_storage,reserved_storage) VALUES(7,'candidate_fixture','合成账号','fixture_hash',1024,?)", fileSize)
	execute("INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(11,7,'28ee758a-0727-42de-ae7d-3bb33155e148','fixture','WEB')")
	execute("INSERT INTO file_entries(id,user_id,name,name_key,entry_type) VALUES(13,7,'',X'','DIR')")
	execute("INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(23,7,?,?,?,?)", settings.Provider, settings.Bucket, key, fileSize)
	execute(`INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,storage_upload_id,
		expected_sha256,file_size,part_size,part_count,status,reserved_bytes,expires_at)
		VALUES(19,7,11,23,13,'fixture.bin','MULTIPART',?,UNHEX(REPEAT('ab',32)),?,?,2,'UPLOADING',?,UTC_TIMESTAMP(6)+INTERVAL 1 DAY)`,
		sessions[0], fileSize, partSize, fileSize)
	targets, err := store.ListMultipartUploadSessionScanTargets(ctx, settings.Provider, settings.Bucket, 0, 1)
	if err != nil || targets == nil || len(targets.Targets) != 1 {
		t.Fatal("未取得原任务的对象键", err)
	}
	target := targets.Targets[0]
	scanner := service.NewMultipartUploadSessionCandidateScanner(settings.Provider, settings.Bucket, client.ListMultipartUploadSessionsPage,
		store.ReadMultipartUploadSessionCleanupFacts)

	// 2. 用一页预算暂停，再从保存的游标继续读到末页。
	const grace = 15 * time.Minute
	first, err := scanner.ScanPages(ctx, target, "", 2, 1, grace)
	if err != nil || first.PagesScanned != 1 || first.Scanned != 2 || first.SkippedBound != 1 || len(first.Failures) != 0 || len(first.Candidates) != 1 ||
		first.Candidates[0].UploadID != sessions[1] || first.NextUploadID != sessions[1] {
		t.Fatal("第一页绑定或超期结果不符", err)
	}
	second, err := scanner.ScanPages(ctx, target, first.NextUploadID, 2, 2, grace)
	if err != nil || second.PagesScanned != 1 || second.Scanned != 1 || len(second.Failures) != 0 || len(second.Candidates) != 1 ||
		second.Candidates[0].UploadID != sessions[2] || second.NextUploadID != "" {
		t.Fatal("第二页未按原游标续读", err)
	}
	// 3. 在同一轮跨页汇总不同保护，再确认任务与会话仍可读取。
	fresh, err := scanner.ScanPages(ctx, target, "", 2, 2, 2*time.Hour)
	if err != nil || fresh.PagesScanned != 2 || fresh.Scanned != 3 || fresh.NextUploadID != "" || fresh.SkippedBound != 1 ||
		fresh.SkippedWithinGrace != 2 || len(fresh.Candidates) != 0 || len(fresh.Failures) != 0 {
		t.Fatal("仍在宽限期的会话未正确跳过", err)
	}
	execute("UPDATE upload_tasks SET status='CREATING',storage_upload_id=NULL,lease_owner='fixture-creator',lease_version=1,lease_until=UTC_TIMESTAMP(6)+INTERVAL 1 MINUTE WHERE id=19")
	before, err := store.FindUserUploadTask(ctx, 7, 19)
	if err != nil {
		t.Fatal(err)
	}
	protected, err := scanner.ScanPages(ctx, target, "", 2, 2, grace)
	if err != nil || protected.PagesScanned != 2 || protected.Scanned != 3 || protected.NextUploadID != "" ||
		protected.SkippedCreation != 3 || len(protected.Candidates) != 0 || len(protected.Failures) != 0 {
		t.Fatal("有效创建租约未保护原键下的会话", err)
	}
	after, err := store.FindUserUploadTask(ctx, 7, 19)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("只读扫描改变了任务记录", err)
	}
	for _, uploadID := range sessions {
		if _, err := client.ListUploadPartsPage(ctx, key, uploadID, 0, 1); err != nil {
			t.Fatal("只读复核后未能读到原会话", err)
		}
	}
	t.Log("一页预算后按原游标续扫，同轮两页汇总保护，三个合成会话仍可读")
}
