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
	// 1. 准备独占的 MySQL 库和 MinIO 桶，创建一个真实到期对象。
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
	const key = "users/7/objects/gc-writeback-fixture"
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
		if err := verifier.RemoveObject(cleanupContext, bucket, key, minio.RemoveObjectOptions{VersionID: "null"}); err != nil {
			t.Error("清理独占对象失败", minio.ToErrorResponse(err).Code)
			return
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
	data := []byte("retired fixture content")
	receipt, err := client.PutUploadContent(ctx, key, bytes.NewReader(data), uint64(len(data)))
	if err != nil {
		t.Fatal("写入待回收内容失败", err)
	}
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal("设置删除恢复夹具失败", err)
		}
	}
	digest := sha256.Sum256(data)
	execute(`INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,storage_version_id,file_size,
		verified_sha256,status,storage_etag,verified_at,unreferenced_at,gc_after)
		VALUES(33,7,?,?,?,?,?,?,'READY',?,UTC_TIMESTAMP(6)-INTERVAL 9 DAY,
		       UTC_TIMESTAMP(6)-INTERVAL 8 DAY,UTC_TIMESTAMP(6)-INTERVAL 1 DAY)`,
		settings.Provider, bucket, key, receipt.StorageVersionID, receipt.SizeBytes, digest[:], receipt.StorageETag)
	candidates, err := store.ListStoredObjectGCCandidates(ctx, 1)
	if err != nil || len(candidates) != 1 || candidates[0].ObjectID != 33 {
		t.Fatal("未取得真实删除候选", candidates, err)
	}
	candidate := candidates[0]

	// 2. 从待领取候选执行真实删除，只阻断完成写回，保留已提交的删除意图。
	execute(`CREATE TRIGGER fixture_fail_object_deletion_writeback BEFORE UPDATE ON stored_objects
		FOR EACH ROW BEGIN
			IF NEW.status = 'DELETED' THEN
				SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'fixture blocks deletion writeback';
			END IF;
		END`)
	processor := service.NewStoredObjectDeletionProcessor(client.DeleteStoredObject, store.CompleteStoredObjectGC)
	candidateProcessor := service.NewStoredObjectGCCandidateProcessor(store.ClaimStoredObjectGC, processor.Process)
	completed, err := candidateProcessor.Process(ctx, candidate)
	var writeError *mysql.MySQLError
	if completed != nil || !errors.As(err, &writeError) || writeError.Number != 1644 {
		t.Fatal("未保留写回失败原因", completed, err)
	}
	if _, err := verifier.StatObject(ctx, bucket, key, minio.StatObjectOptions{}); minio.ToErrorResponse(err).StatusCode != http.StatusNotFound {
		t.Fatal("第一次处理后未确认物理目标缺失", minio.ToErrorResponse(err).Code)
	}
	pending, err := store.ListDeletingStoredObjects(ctx, 1)
	if err != nil || len(pending) != 1 {
		t.Fatal("写回失败后未保留待恢复删除记录", pending, err)
	}
	claimed := &pending[0]
	if claimed.ID != candidate.ObjectID || claimed.OwnerUserID != candidate.OwnerUserID ||
		claimed.Status != domain.StoredObjectStatusDeleting || claimed.Bucket != bucket || claimed.ObjectKey != key ||
		claimed.UnreferencedAt == nil || !claimed.UnreferencedAt.Equal(candidate.UnreferencedAt) ||
		claimed.GCAfter == nil || !claimed.GCAfter.Equal(candidate.GCAfter) {
		t.Fatal("待恢复记录与已领取候选不符", claimed)
	}
	if result, err := candidateProcessor.Process(ctx, candidate); result != nil || !errors.Is(err, domain.ErrStoredObjectGCCandidateStale) {
		t.Fatal("已领取候选应失效", result, err)
	}

	// 3. 恢复批次先遇到无法定位的旧记录，再处理已经物理删除的对象。
	execute("DROP TRIGGER fixture_fail_object_deletion_writeback")
	execute(`INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size,status)
		VALUES(34,8,'fixture_other_provider',?,'users/8/objects/unavailable-location',0,'DELETING')`, bucket)
	batch := service.NewStoredObjectDeletionRecoveryBatch(store.ListDeletingStoredObjects, processor.Process)
	summary, err := batch.RunOnce(ctx, 2)
	if err != nil || summary.Scanned != 2 || summary.Completed != 1 || len(summary.CandidateErrors) != 1 {
		t.Fatal("删除恢复批次结果不符", summary, err)
	}
	failure := summary.CandidateErrors[0]
	if failure.ObjectID != 34 || failure.OwnerUserID != 8 || !errors.Is(failure.Err, domain.ErrInvalidStoredObjectData) {
		t.Fatal("未正确记录无法定位的旧对象", failure)
	}
	current, err := store.FindUserStoredObject(ctx, claimed.OwnerUserID, claimed.ID)
	if err != nil || current == nil || current.Status != domain.StoredObjectStatusDeleted || !sameStoredObjectLocation(current, claimed) {
		t.Fatal("批次未提交已缺失对象的完成状态", current, err)
	}
	if pending, err := store.ListDeletingStoredObjects(ctx, 2); err != nil || len(pending) != 1 || pending[0].ID != failure.ObjectID {
		t.Fatal("恢复扫描应只保留本轮失败对象", pending, err)
	}
}
