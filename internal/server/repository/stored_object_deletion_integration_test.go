package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/internal/server/storage"
)

func TestStoredObjectDeletionRecoveryIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_UPLOAD_STORAGE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_UPLOAD_STORAGE_TEST=1 后在独占桶验证物理删除恢复")
	}
	// 1. 准备独占的 MySQL 库和 MinIO 桶，创建三个到期对象和一个稍后到期的对象。
	pool, _ := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := config.LoadStorageConfig()
	if err != nil {
		t.Fatal(err)
	}
	const bucketPrefix = "cloudnest-gc-it-"
	bucket := bucketPrefix + uuid.NewString()
	const writebackKey = "users/7/objects/gc-writeback-fixture"
	const staleKey = "users/7/objects/gc-stale-fixture"
	const successKey = "users/7/objects/gc-success-fixture"
	const nextDueKey = "users/7/objects/gc-next-due-fixture"
	fixtures := []struct {
		id          uint64
		key         string
		daysOverdue int
	}{{33, writebackKey, 3}, {35, staleKey, 2}, {36, successKey, 1}, {37, nextDueKey, -1}}
	secure := settings.EndpointURL.Scheme == "https"
	transport, err := minio.DefaultTransport(secure)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(transport.CloseIdleConnections)
	verifier, err := minio.New(settings.EndpointURL.Host, &minio.Options{
		Secure: secure, Creds: credentials.NewStaticV4(settings.AccessKey, settings.SecretKey, ""),
		Transport: transport, BucketLookup: minio.BucketLookupPath, MaxRetries: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatal("创建独占回收测试桶失败", minio.ToErrorResponse(err).Code)
	}
	t.Cleanup(func() {
		if _, err := uuid.Parse(strings.TrimPrefix(bucket, bucketPrefix)); err != nil || !strings.HasPrefix(bucket, bucketPrefix) {
			t.Error("拒绝清理非本轮独占桶")
			return
		}
		cleanupContext, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		for _, fixture := range fixtures {
			if err := verifier.RemoveObject(cleanupContext, bucket, fixture.key, minio.RemoveObjectOptions{VersionID: "null"}); err != nil {
				t.Error("清理独占对象失败", minio.ToErrorResponse(err).Code)
			}
		}
		if err := verifier.RemoveBucket(cleanupContext, bucket); err != nil {
			t.Error("清理独占桶失败", minio.ToErrorResponse(err).Code)
		}
		if exists, err := verifier.BucketExists(cleanupContext, bucket); err != nil || exists {
			t.Error("独占桶清理复核失败")
		}
	})
	settings.Bucket = bucket
	client, err := storage.NewClient(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal("设置删除恢复夹具失败", err)
		}
	}
	for _, fixture := range fixtures {
		data := []byte(fixture.key)
		receipt, err := client.PutUploadContent(ctx, fixture.key, bytes.NewReader(data), uint64(len(data)))
		if err != nil {
			t.Fatal("写入待回收内容失败", err)
		}
		digest := sha256.Sum256(data)
		execute(`INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,storage_version_id,file_size,
			verified_sha256,status,storage_etag,verified_at,unreferenced_at,gc_after)
			VALUES(?,7,?,?,?,?,?,?,'READY',?,UTC_TIMESTAMP(6)-INTERVAL 12 DAY,
			       UTC_TIMESTAMP(6)-INTERVAL ? DAY,UTC_TIMESTAMP(6)-INTERVAL ? DAY)`,
			fixture.id, settings.Provider, bucket, fixture.key, receipt.StorageVersionID, receipt.SizeBytes,
			digest[:], receipt.StorageETag, fixture.daysOverdue+7, fixture.daysOverdue)
	}

	// 2. 跑到期批次，模拟中间候选计时被清空，并阻断第一个对象的完成写回。
	execute(`CREATE TRIGGER fixture_fail_object_deletion_writeback BEFORE UPDATE ON stored_objects
		FOR EACH ROW BEGIN
			IF NEW.id = 33 AND NEW.status = 'DELETED' THEN
				SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'fixture blocks deletion writeback';
			END IF;
		END`)
	processor := service.NewStoredObjectDeletionProcessor(client.DeleteStoredObject, store.CompleteStoredObjectGC)
	candidateProcessor := service.NewStoredObjectGCCandidateProcessor(store.ClaimStoredObjectGC, processor.Process)
	var writebackCandidate domain.StoredObjectGCCandidate
	dueBatch := service.NewStoredObjectGCBatch(func(scanContext context.Context, limit int) ([]domain.StoredObjectGCCandidate, error) {
		candidates, err := store.ListStoredObjectGCCandidates(scanContext, limit)
		if err != nil || len(candidates) != 3 || candidates[0].ObjectID != 33 || candidates[1].ObjectID != 35 || candidates[2].ObjectID != 36 {
			t.Fatal("未按到期时间取得三个真实候选", candidates, err)
		}
		writebackCandidate = candidates[0]
		execute("UPDATE stored_objects SET unreferenced_at = NULL, gc_after = NULL WHERE owner_user_id = 7 AND id = 35")
		return candidates, nil
	}, candidateProcessor.Process)
	dueSummary, err := dueBatch.RunOnce(ctx, len(fixtures))
	if err != nil || dueSummary.Scanned != 3 || dueSummary.Completed != 1 || dueSummary.StaleCandidates != 1 || len(dueSummary.CandidateErrors) != 1 {
		t.Fatal("到期批次结果不符", dueSummary, err)
	}
	dueFailure := dueSummary.CandidateErrors[0]
	var writeError *mysql.MySQLError
	if dueFailure.ObjectID != 33 || dueFailure.OwnerUserID != 7 || !errors.As(dueFailure.Err, &writeError) || writeError.Number != 1644 {
		t.Fatal("未保留写回失败原因", dueFailure)
	}
	for _, key := range []string{writebackKey, successKey} {
		if _, err := verifier.StatObject(ctx, bucket, key, minio.StatObjectOptions{}); minio.ToErrorResponse(err).StatusCode != http.StatusNotFound {
			t.Fatal("批次处理后目标仍存在或查询异常", key, minio.ToErrorResponse(err).Code)
		}
	}
	if _, err := verifier.StatObject(ctx, bucket, staleKey, minio.StatObjectOptions{}); err != nil {
		t.Fatal("候选失效后应保留原内容", minio.ToErrorResponse(err).Code)
	}
	completed, err := store.FindUserStoredObject(ctx, 7, 36)
	if err != nil || completed == nil || completed.Status != domain.StoredObjectStatusDeleted {
		t.Fatal("批次未写回已完成对象", completed, err)
	}
	pending, err := store.ListDeletingStoredObjects(ctx, 1)
	if err != nil || len(pending) != 1 {
		t.Fatal("写回失败后未保留待恢复删除记录", pending, err)
	}
	claimed := &pending[0]
	if claimed.ID != writebackCandidate.ObjectID || claimed.OwnerUserID != writebackCandidate.OwnerUserID ||
		claimed.Status != domain.StoredObjectStatusDeleting || claimed.Bucket != bucket || claimed.ObjectKey != writebackKey ||
		claimed.UnreferencedAt == nil || !claimed.UnreferencedAt.Equal(writebackCandidate.UnreferencedAt) ||
		claimed.GCAfter == nil || !claimed.GCAfter.Equal(writebackCandidate.GCAfter) {
		t.Fatal("待恢复记录与已领取候选不符", claimed)
	}
	if result, err := candidateProcessor.Process(ctx, writebackCandidate); result != nil || !errors.Is(err, domain.ErrStoredObjectGCCandidateStale) {
		t.Fatal("已领取候选应失效", result, err)
	}

	// 3. 用统一入口先恢复旧删除记录，再处理另一个刚到期的对象。
	execute("DROP TRIGGER fixture_fail_object_deletion_writeback")
	execute(`INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size,status)
		VALUES(34,8,'fixture_other_provider',?,'users/8/objects/unavailable-location',0,'DELETING')`, bucket)
	execute(`UPDATE stored_objects
		SET unreferenced_at = UTC_TIMESTAMP(6)-INTERVAL 8 DAY, gc_after = UTC_TIMESTAMP(6)-INTERVAL 1 DAY
		WHERE owner_user_id = 7 AND id = 37`)
	recoveryBatch := service.NewStoredObjectDeletionRecoveryBatch(store.ListDeletingStoredObjects, processor.Process)
	nextDueBatch := service.NewStoredObjectGCBatch(func(scanContext context.Context, limit int) ([]domain.StoredObjectGCCandidate, error) {
		current, err := store.FindUserStoredObject(scanContext, claimed.OwnerUserID, claimed.ID)
		if err != nil || current == nil || current.Status != domain.StoredObjectStatusDeleted || !sameStoredObjectLocation(current, claimed) {
			t.Fatal("扫描新候选前应已写回旧对象的删除状态", current, err)
		}
		return store.ListStoredObjectGCCandidates(scanContext, limit)
	}, candidateProcessor.Process)
	round := service.NewStoredObjectGCRound(recoveryBatch.RunOnce, nextDueBatch.RunOnce)
	result, err := round.RunOnce(ctx, 2, 1)
	recovery := result.Recovery
	if err != nil || recovery.Scanned != 2 || recovery.Completed != 1 || len(recovery.CandidateErrors) != 1 {
		t.Fatal("删除恢复批次结果不符", result, err)
	}
	failure := recovery.CandidateErrors[0]
	if failure.ObjectID != 34 || failure.OwnerUserID != 8 || !errors.Is(failure.Err, domain.ErrInvalidStoredObjectData) {
		t.Fatal("未正确记录无法定位的旧对象", failure)
	}
	if result.Due.Scanned != 1 || result.Due.Completed != 1 || result.Due.StaleCandidates != 0 || len(result.Due.CandidateErrors) != 0 {
		t.Fatal("统一入口未完成新到期对象", result.Due)
	}
	nextCompleted, err := store.FindUserStoredObject(ctx, 7, 37)
	if err != nil || nextCompleted == nil || nextCompleted.Status != domain.StoredObjectStatusDeleted {
		t.Fatal("未提交新到期对象的完成状态", nextCompleted, err)
	}
	if _, err := verifier.StatObject(ctx, bucket, nextDueKey, minio.StatObjectOptions{}); minio.ToErrorResponse(err).StatusCode != http.StatusNotFound {
		t.Fatal("未确认新到期对象的物理内容缺失", minio.ToErrorResponse(err).Code)
	}
	if pending, err := store.ListDeletingStoredObjects(ctx, 2); err != nil || len(pending) != 1 || pending[0].ID != failure.ObjectID {
		t.Fatal("恢复扫描应只保留本轮失败对象", pending, err)
	}
}
