package main

import (
	"bytes"
	"context"
	"crypto/sha256"
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

func TestServerMultipartCancellationAssemblyIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MULTIPART_CANCEL_ASSEMBLY_TEST") != "1" {
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

	// 1. 准备两条合成任务，一条未绑定且无对象，另一条同时有会话和对象版本。
	const partSize, fileSize = 16 << 20, (16 << 20) + 1
	const boundKey, unboundKey = "b4/cancel-assembly-bound.bin", "b4/cancel-assembly-unbound.bin"
	const userID, boundTaskID, unboundTaskID = 7, 19, 20
	const boundObjectID, unboundObjectID = 23, 24
	const usedBytes, otherReserved = 1024, 512
	payload := bytes.Repeat([]byte{0x42}, fileSize)
	digest := sha256.Sum256(payload)
	version, err := bucket.client.PutObject(ctx, bucket.name, boundKey, bytes.NewReader(payload), fileSize, minio.PutObjectOptions{DisableMultipart: true})
	if err != nil || version.VersionID == "" || version.VersionID == "null" || version.ETag == "" {
		t.Fatal("未写入合成对象的确切版本", err)
	}
	session, err := client.CreateMultipartUpload(ctx, boundKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		core := minio.Core{Client: bucket.client}
		if err := core.AbortMultipartUpload(cleanup, bucket.name, boundKey, session); err != nil && minio.ToErrorResponse(err).Code != "NoSuchUpload" {
			t.Error("专用会话清理失败", minio.ToErrorResponse(err).Code)
		}
	})
	if _, err := client.ListUploadPartsPage(ctx, boundKey, session, 0, 1); err != nil {
		t.Fatal("首轮前原会话不可用", err)
	}
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	assertQuota := func(wantReserved uint64) {
		t.Helper()
		var used, reserved uint64
		if err := pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage FROM users WHERE id=?", userID).Scan(&used, &reserved); err != nil ||
			used != usedBytes || reserved != wantReserved {
			t.Fatal("已用空间或预占不符", used, reserved, wantReserved, err)
		}
	}
	start := time.Now().UTC().Truncate(time.Second)
	execute("SET timestamp = ?", start.Unix())
	execute("INSERT INTO users(id,username,display_name,password_hash,used_storage,reserved_storage) VALUES(7,'assembly_fixture','取消装配','fixture_hash',?,?)", usedBytes, 2*fileSize+otherReserved)
	execute("INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(11,7,'28ee758a-0727-42de-ae7d-3bb33155e148','fixture','WEB')")
	execute("INSERT INTO file_entries(id,user_id,name,name_key,entry_type) VALUES(13,7,'',X'','DIR')")
	execute("INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(?,7,?,?,?,?),(?,7,?,?,?,?)",
		boundObjectID, settings.Provider, settings.Bucket, boundKey, fileSize, unboundObjectID, settings.Provider, settings.Bucket, unboundKey, fileSize)
	execute(`INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,storage_upload_id,
		expected_sha256,file_size,part_size,part_count,status,reserved_bytes,expires_at) VALUES
		(?,7,11,?,13,'bound.bin','MULTIPART',?,?,?,?,2,'CANCELLING',?,UTC_TIMESTAMP(6)-INTERVAL 1 SECOND),
		(?,7,11,?,13,'unbound.bin','MULTIPART',NULL,?,?,?,2,'CANCELLING',?,UTC_TIMESTAMP(6)-INTERVAL 1 SECOND)`,
		boundTaskID, boundObjectID, session, digest[:], fileSize, partSize, fileSize, unboundTaskID, unboundObjectID, digest[:], fileSize, partSize, fileSize)

	// 2. 无受信实例确认时，已绑定任务登记重试，未绑定任务按缺失对象收尾。
	withoutVerifier := newServerMultipartUploadCancellationBatch(store, client, settings, nil)
	assertRound := func(batch *service.MultipartUploadCancellationBatch, owner string, scanned, cancelled, retries int) {
		t.Helper()
		summary, err := batch.RunOnce(ctx, 2, owner)
		if err != nil || summary.Scanned != scanned || summary.Cancelled != cancelled || summary.RetriesScheduled != retries ||
			summary.Skipped != 0 || len(summary.CandidateErrors) != 0 {
			t.Fatal("取消批次结果不符", summary, err)
		}
	}
	assertRound(withoutVerifier, "fixture-no-verifier", 2, 1, 1)
	pending, err := store.FindUserUploadTask(ctx, userID, boundTaskID)
	if err != nil || pending.Status != domain.UploadStatusCancelling || pending.ReservedBytes != fileSize ||
		pending.NextAttemptAt == nil || !pending.NextAttemptAt.Equal(start.Add(time.Minute)) || pending.AttemptCount != 1 || pending.LeaseVersion != 1 ||
		pending.LeaseOwner != nil || pending.LeaseUntil != nil || pending.LastErrorCode == nil || *pending.LastErrorCode != "STORAGE_UNAVAILABLE" {
		t.Fatal("已绑定任务未按原租约截止登记重试", err)
	}
	staging, err := store.FindUserStoredObject(ctx, userID, boundObjectID)
	if err != nil || staging.Status != domain.StoredObjectStatusStaging || staging.GCAfter != nil {
		t.Fatal("待确认的对象已变更状态", err)
	}
	if _, err := client.ListUploadPartsPage(ctx, boundKey, session, 0, 1); !errors.Is(err, domain.ErrUploadPartSessionMissing) {
		t.Fatal("未真实中止原上传会话", err)
	}
	missing, err := store.FindUserUploadTask(ctx, userID, unboundTaskID)
	if err != nil || missing.Status != domain.UploadStatusCancelled || missing.ReservedBytes != 0 {
		t.Fatal("未绑定任务未结束取消", err)
	}
	missingObject, err := store.FindUserStoredObject(ctx, userID, unboundObjectID)
	if err != nil || missingObject.Status != domain.StoredObjectStatusOrphaned || missingObject.GCAfter == nil ||
		missingObject.StorageVersionID != nil || missingObject.StorageETag != nil {
		t.Fatal("缺失对象的回收登记不符", err)
	}
	assertQuota(fileSize + otherReserved)
	t.Logf("首轮取消未绑定任务 %d，任务 %d 已中止并登记 STORAGE_UNAVAILABLE，账号预占 %d B", unboundTaskID, boundTaskID, fileSize+otherReserved)

	// 3. 到期后用同一隔离实例的受信证明重新运行，核对原版本和单次返还。
	withVerifier := newServerMultipartUploadCancellationBatch(store, client, settings, isolatedMinIOVerifier(minioID))
	execute("SET timestamp = ?", pending.NextAttemptAt.Add(-time.Second).Unix())
	assertRound(withVerifier, "fixture-verified", 0, 0, 0)
	assertQuota(fileSize + otherReserved)
	execute("SET timestamp = ?", pending.NextAttemptAt.Unix())
	assertRound(withVerifier, "fixture-verified", 1, 1, 0)
	finished, err := store.FindUserUploadTask(ctx, userID, boundTaskID)
	if err != nil || finished.Status != domain.UploadStatusCancelled || finished.ReservedBytes != 0 || finished.LeaseVersion != 2 {
		t.Fatal("受信复查未按新租约完成取消", err)
	}
	orphaned, err := store.FindUserStoredObject(ctx, userID, boundObjectID)
	if err != nil || orphaned.Status != domain.StoredObjectStatusOrphaned || orphaned.UnreferencedAt == nil || orphaned.GCAfter == nil ||
		!orphaned.GCAfter.Equal(orphaned.UnreferencedAt.Add(7*24*time.Hour)) || orphaned.StorageVersionID == nil ||
		*orphaned.StorageVersionID != version.VersionID || orphaned.StorageETag == nil || *orphaned.StorageETag != version.ETag {
		t.Fatal("未按原对象版本登记七天回收宽限", err)
	}
	stored, err := bucket.client.StatObject(ctx, bucket.name, boundKey, minio.StatObjectOptions{VersionID: version.VersionID})
	if err != nil || stored.VersionID != version.VersionID || stored.ETag != version.ETag || stored.Size != fileSize {
		t.Fatal("取消收尾改变了原存储版本", err)
	}
	assertQuota(otherReserved)
	assertRound(withVerifier, "fixture-verified", 0, 0, 0)
	assertQuota(otherReserved)
	t.Logf("到期重领版本 %d 后任务 %d 取消完成，原对象版本仍可读取，已用 %d B、预占 %d B", finished.LeaseVersion, boundTaskID, usedBytes, otherReserved)
}
