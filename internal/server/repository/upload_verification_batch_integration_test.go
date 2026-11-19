package repository

import (
	"context"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestUploadVerificationBatchIntegration(t *testing.T) {
	// 1. 准备四条待校验任务，以乱序插入验证扫描顺序和上限。
	pool, original := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	const failedID, publishedID, retryID, deferredID uint64 = 100, 101, 102, 103
	for _, taskID := range []uint64{deferredID, publishedID, failedID, retryID} {
		suffix := strconv.FormatUint(taskID, 10)
		execute(`INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size,status)
			VALUES(?,?,'local_minio','fixture-bucket',?,0,'VERIFYING')`, taskID, original.UserID, "batch-"+suffix)
		execute(`INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,file_size,status,reserved_bytes,expires_at)
			VALUES(?,?,?,?,?,?,'SINGLE',0,'VERIFYING',0,UTC_TIMESTAMP(6)+INTERVAL 1 DAY)`,
			taskID, original.UserID, original.DeviceID, taskID, original.TargetParentID, "batch-"+suffix+".bin")
	}

	// 2. 使用真实扫描、领取和结果写回，按对象提供不同的受控读回结果。
	publisher := service.NewUploadPublicationService(func(ctx context.Context, task *domain.UploadTask, staged *domain.StoredObject, digest domain.UploadContentDigest,
		publish func(context.Context, service.UploadPublicationTransaction, *domain.UploadTask, *domain.StoredObject) error) error {
		return store.WithUploadVerificationTransaction(ctx, task, staged, digest, func(txContext context.Context, txStore *Store, current *domain.UploadTask, object *domain.StoredObject) error {
			return publish(txContext, txStore, current, object)
		})
	})
	var opened []uint64
	verification := service.NewUploadVerificationService(func(_ context.Context, object *domain.StoredObject) (io.ReadCloser, error) {
		opened = append(opened, object.ID)
		switch object.ID {
		case failedID:
			return io.NopCloser(strings.NewReader("x")), nil
		case retryID:
			return nil, domain.ErrUploadStorageUnavailable
		default:
			return io.NopCloser(strings.NewReader("")), nil
		}
	}, publisher.Publish, store.FailUploadForChecksumMismatch, store.FailUploadForBodySizeMismatch, store.ScheduleUploadVerificationRetry)
	processor := service.NewUploadVerificationCandidateProcessor(store.ClaimUploadVerificationLease, store.FindUserStoredObject, verification.VerifyAndPublish)
	scanCalls := 0
	batch := service.NewUploadVerificationBatch(func(ctx context.Context, limit int) ([]domain.UploadVerificationCandidate, error) {
		scanCalls++
		return store.ListUploadVerificationCandidates(ctx, limit)
	}, processor.Process)
	summary, err := batch.RunOnce(ctx, 3, "batch-worker")
	if err != nil || scanCalls != 1 || summary.Scanned != 3 || summary.Published != 1 || summary.RetriesScheduled != 1 || len(summary.CandidateErrors) != 1 {
		t.Fatal("单轮扫描的次数或汇总错误", summary, scanCalls, err)
	}
	if summary.CandidateErrors[0].TaskID != failedID || !errors.Is(summary.CandidateErrors[0].Err, domain.ErrUploadBodySizeMismatch) ||
		!slices.Equal(opened, []uint64{failedID, publishedID, retryID}) {
		t.Fatal("未按顺序处理或丢失了失败原因", summary.CandidateErrors, opened)
	}

	// 3. 从事务外核对三条结果和上限之外候选的租约版本。
	readTask := func(taskID uint64) *domain.UploadTask {
		t.Helper()
		task, err := store.FindUserUploadTask(ctx, original.UserID, taskID)
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	failed, published, retry, deferred := readTask(failedID), readTask(publishedID), readTask(retryID), readTask(deferredID)
	if failed.Status != domain.UploadStatusFailed || failed.LastErrorCode == nil || *failed.LastErrorCode != domain.UploadBodySizeMismatchCode || failed.LeaseVersion != 1 {
		t.Fatal("失败任务未提交正确终态", failed)
	}
	if published.Status != domain.UploadStatusCompleted || published.ResultEntryID == nil || published.LeaseVersion != 1 {
		t.Fatal("前一条失败影响了后续发布", published)
	}
	if retry.Status != domain.UploadStatusVerifying || retry.AttemptCount != 1 || retry.NextAttemptAt == nil || retry.LeaseOwner != nil || retry.LeaseVersion != 1 {
		t.Fatal("重试未提交正确记录", retry)
	}
	if deferred.Status != domain.UploadStatusVerifying || deferred.NextAttemptAt != nil || deferred.LeaseOwner != nil || deferred.LeaseVersion != 0 {
		t.Fatal("扫描上限之外的任务发生了变化", deferred)
	}
}
