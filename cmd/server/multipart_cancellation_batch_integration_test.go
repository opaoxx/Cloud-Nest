package main

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/internal/server/storage"
)

func TestServerMultipartCancellationBatchIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MULTIPART_CANCEL_BATCH_TEST") != "1" {
		t.Skip("显式启用后创建本轮专用 MySQL、MinIO 和合成数据")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	mysqlID := startIsolatedWebMySQL(t, ctx)
	t.Logf("专用 MySQL %s，回环地址 %s", mysqlID[:12], os.Getenv("CLOUDNEST_DB_ADDRESS"))
	minioID := startIsolatedWebMinIO(t, ctx)
	t.Logf("专用 MinIO %s，回环地址 %s", minioID[:12], os.Getenv("CLOUDNEST_S3_ENDPOINT"))
	bucket := prepareServerUploadContentBucket(t, ctx)
	pool, _ := prepareServerIntegrationDatabase(t, ctx)
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

	// 1. 准备跨两个账号的五个合成候选，首轮限量四条。
	const partSize, fileSize = 16 << 20, (16 << 20) + 1
	const otherReserved = 512
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	execute(`INSERT INTO users(id,username,display_name,password_hash,used_storage,reserved_storage) VALUES
		(7,'batch_owner','批次账号','fixture_hash',1024,?),(8,'batch_other','第二个批次账号','fixture_hash',2048,?)`,
		3*fileSize+otherReserved, 2*fileSize+otherReserved)
	execute(`INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES
		(11,7,'28ee758a-0727-42de-ae7d-3bb33155e148','fixture-a','WEB'),
		(12,8,'38ee758a-0727-42de-ae7d-3bb33155e148','fixture-b','WEB')`)
	execute("INSERT INTO file_entries(id,user_id,name,name_key,entry_type) VALUES(13,7,'',X'','DIR'),(14,8,'',X'','DIR')")
	const retryKey = "b4/batch-retry.bin"
	session, err := client.CreateMultipartUpload(ctx, retryKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		core := minio.Core{Client: bucket.client}
		if err := core.AbortMultipartUpload(cleanup, bucket.name, retryKey, session); err != nil && minio.ToErrorResponse(err).Code != "NoSuchUpload" {
			t.Error("专用会话清理失败", minio.ToErrorResponse(err).Code)
		}
	})
	fixtures := []struct {
		taskID, userID, objectID uint64
		key                      string
		binding                  *string
	}{
		{19, 7, 21, "b4/batch-stale.bin", nil},
		{20, 7, 22, "b4/batch-referenced.bin", nil},
		{21, 8, 23, retryKey, &session},
		{22, 8, 24, "b4/batch-cancelled.bin", nil},
		{23, 7, 25, "b4/batch-next-round.bin", nil},
	}
	for _, fixture := range fixtures {
		execute("INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(?,?,?,?,?,?)",
			fixture.objectID, fixture.userID, settings.Provider, settings.Bucket, fixture.key, fileSize)
		execute(`INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,storage_upload_id,
			expected_sha256,file_size,part_size,part_count,status,reserved_bytes,expires_at)
			VALUES(?,?,?,?,?,'fixture.bin','MULTIPART',?,UNHEX(REPEAT('ab',32)),?,?,2,'CANCELLING',?,UTC_TIMESTAMP(6)-INTERVAL 1 SECOND)`,
			fixture.taskID, fixture.userID, fixture.userID+4, fixture.objectID, fixture.userID+6, fixture.binding, fileSize, partSize, fileSize)
	}
	execute(`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size)
		VALUES(31,7,13,'fixture.bin',CAST('fixture.bin' AS BINARY),'FILE','OTHER',22,?)`, fileSize)

	// 2. 使用正式读取、租约、存储执行和写回。第一次领取前让已扫描候选失效。
	observations := service.NewMultipartUploadCancellationStorageService(
		func(callContext context.Context, claimed *domain.UploadTask) (*domain.MultipartUploadCancellationTarget, error) {
			return store.FindMultipartUploadCancellationTarget(callContext, claimed, settings.Provider, settings.Bucket)
		}, client.AbortMultipartUpload,
		func(callContext context.Context, key, session string) (*domain.MultipartUploadAbortObservation, error) {
			return client.CheckMultipartUploadAfterAbort(callContext, key, session, nil)
		}, client.StatMultipartUploadObject)
	execution := service.NewMultipartUploadCancellationExecutionService(settings.Provider, settings.Bucket,
		observations.ObserveBoundSession, observations.ObserveUnboundSession,
		store.CompleteMissingMultipartUploadCancellation, store.CompleteExistingMultipartUploadCancellation,
		store.ScheduleMultipartUploadCancellationRetry)
	batch := service.NewMultipartUploadCancellationBatch(store.ListMultipartUploadCancellationCandidates,
		func(callContext context.Context, candidate domain.UploadCancellationCandidate, owner string) (*domain.UploadTask, error) {
			if candidate.TaskID == 19 {
				execute("UPDATE upload_tasks SET status='EXPIRING' WHERE user_id=7 AND id=19")
			}
			return store.ClaimMultipartUploadCancellationLease(callContext, candidate, owner)
		}, execution.Execute)
	summary, err := batch.RunOnce(ctx, 4, "fixture-batch")
	if err != nil || summary.Scanned != 4 || summary.Cancelled != 1 || summary.RetriesScheduled != 1 || summary.Skipped != 1 ||
		len(summary.CandidateErrors) != 1 || summary.CandidateErrors[0].TaskID != 20 ||
		!errors.Is(summary.CandidateErrors[0].Err, domain.ErrStoredObjectInUse) {
		t.Fatal("混合批次统计或错误不符", summary, err)
	}

	// 3. 逐项回读和核对账目，确认本轮上限及失败回滚。
	for index, fixture := range fixtures {
		record, err := store.FindUserUploadTask(ctx, fixture.userID, fixture.taskID)
		if err != nil {
			t.Fatal(err)
		}
		wantStatus := []domain.UploadStatus{domain.UploadStatusExpiring, domain.UploadStatusCancelling,
			domain.UploadStatusCancelling, domain.UploadStatusCancelled, domain.UploadStatusCancelling}[index]
		wantReserved := uint64(fileSize)
		if wantStatus == domain.UploadStatusCancelled {
			wantReserved = 0
		}
		if record.Status != wantStatus || record.ReservedBytes != wantReserved {
			t.Fatal("批次实际状态或预占不符", fixture.taskID, record.Status, record.ReservedBytes)
		}
		if fixture.taskID == 21 && (record.NextAttemptAt == nil || record.AttemptCount != 1 || record.LeaseOwner != nil ||
			record.LastErrorCode == nil || *record.LastErrorCode != "STORAGE_UNAVAILABLE") {
			t.Fatal("待确认任务未登记重试")
		}
		if fixture.taskID == 23 && record.LeaseVersion != 0 {
			t.Fatal("超出批量上限的任务被领取")
		}
	}
	stillReferenced, err := store.FindUserStoredObject(ctx, 7, 22)
	if err != nil || stillReferenced.Status != domain.StoredObjectStatusStaging || stillReferenced.GCAfter != nil {
		t.Fatal("错误候选留下了回收登记", err)
	}
	orphaned, err := store.FindUserStoredObject(ctx, 8, 24)
	if err != nil || orphaned.Status != domain.StoredObjectStatusOrphaned || orphaned.GCAfter == nil {
		t.Fatal("已取消候选缺少回收登记", err)
	}
	for _, account := range []struct{ id, used, reserved uint64 }{{7, 1024, 3*fileSize + otherReserved}, {8, 2048, fileSize + otherReserved}} {
		var used, reserved uint64
		if err := pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage FROM users WHERE id=?", account.id).Scan(&used, &reserved); err != nil ||
			used != account.used || reserved != account.reserved {
			t.Fatal("账号账目与批次结果不符", account.id, used, reserved, err)
		}
	}
	next, err := store.ListMultipartUploadCancellationCandidates(ctx, 4)
	if err != nil || len(next) != 1 || next[0].TaskID != 23 {
		t.Fatal("剩余可处理任务与本轮限制不符", next, err)
	}
	t.Log("扫描 4 条，已取消 22、已登记重试 21、跳过 19、任务 20 因对象仍被引用回滚，候选 23 留给下一轮")
}
