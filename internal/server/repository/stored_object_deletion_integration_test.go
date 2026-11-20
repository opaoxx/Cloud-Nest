package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"os"
	"reflect"
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
	claimed, err := store.ClaimStoredObjectGC(ctx, candidates[0])
	if err != nil || claimed == nil {
		t.Fatal("未提交删除意图", claimed, err)
	}

	// 2. 执行真实 MinIO 删除，在写回处注入错误，验证内容已删除但 MySQL 仍可恢复。
	execute(`CREATE TRIGGER fixture_fail_object_deletion_writeback BEFORE UPDATE ON stored_objects
		FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'fixture blocks deletion writeback'`)
	processor := service.NewStoredObjectDeletionProcessor(client.DeleteStoredObject, store.CompleteStoredObjectGC)
	completed, err := processor.Process(ctx, claimed)
	var writeError *mysql.MySQLError
	if completed != nil || !errors.As(err, &writeError) || writeError.Number != 1644 {
		t.Fatal("未保留写回失败原因", completed, err)
	}
	if _, err := verifier.StatObject(ctx, bucket, key, minio.StatObjectOptions{}); minio.ToErrorResponse(err).StatusCode != http.StatusNotFound {
		t.Fatal("第一次处理后未确认物理目标缺失", minio.ToErrorResponse(err).Code)
	}
	pending, err := store.ListDeletingStoredObjects(ctx, 1)
	if err != nil || len(pending) != 1 || !reflect.DeepEqual(pending[0], *claimed) {
		t.Fatal("写回失败后未保留待恢复删除记录", pending, err)
	}

	// 3. 用扫描恢复的记录重试，MinIO 确认目标缺失后提交完成状态。
	execute("DROP TRIGGER fixture_fail_object_deletion_writeback")
	completed, err = processor.Process(ctx, &pending[0])
	if err != nil || completed == nil || completed.Status != domain.StoredObjectStatusDeleted {
		t.Fatal("已缺失对象的删除恢复未完成", completed, err)
	}
	current, err := store.FindUserStoredObject(ctx, claimed.OwnerUserID, claimed.ID)
	if err != nil || !reflect.DeepEqual(current, completed) {
		t.Fatal("写回结果与已提交记录不符", current, err)
	}
	if pending, err := store.ListDeletingStoredObjects(ctx, 1); err != nil || len(pending) != 0 {
		t.Fatal("已完成对象仍出现在恢复扫描", pending, err)
	}
}
