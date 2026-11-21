package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
)

func TestServerMultipartUploadVerificationBatchIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MULTIPART_VERIFICATION_BATCH_TEST") != "1" {
		t.Skip("显式启用后创建本轮专用 MySQL，使用合成对象流")
	}
	// 1. 准备乱序插入的 Multipart 候选和编号更小的 SINGLE 候选。
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	container := startIsolatedWebMySQL(t, ctx)
	t.Logf("专用 MySQL %s，回环地址 %s", container[:12], os.Getenv("CLOUDNEST_DB_ADDRESS"))
	pool, _ := prepareServerIntegrationDatabase(t, ctx)
	store, err := repository.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	execute := func(statement string, arguments ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
			t.Fatal(err)
		}
	}
	const userID, partSize, fileSize = 7, 16 << 20, (16 << 20) + 1
	const singleID, failedID, publishID, retryID, deferredID uint64 = 18, 19, 20, 21, 22
	const singleSize, startUsed = 3, 1024
	execute("INSERT INTO users(id,username,display_name,password_hash,used_storage,reserved_storage) VALUES(7,'multipart_batch_fixture','校验批次','fixture_hash',?,?)",
		startUsed, 4*fileSize+singleSize)
	execute("INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(11,7,'28ee758a-0727-42de-ae7d-3bb33155e148','fixture','WEB')")
	execute("INSERT INTO file_entries(id,user_id,name,name_key,entry_type) VALUES(13,7,'',X'','DIR')")
	content := bytes.Repeat([]byte{0x22}, fileSize)
	checksum := sha256.Sum256(content)
	for _, taskID := range []uint64{deferredID, publishID, failedID, retryID} {
		key := "batch-" + strconv.FormatUint(taskID, 10)
		execute(`INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,storage_version_id,storage_etag,file_size,status)
			VALUES(?,7,'local_minio','fixture-bucket',?,'fixture-version','fixture-etag',?,'VERIFYING')`, taskID, key, fileSize)
		execute(`INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,storage_upload_id,
			expected_sha256,file_size,part_size,part_count,status,reserved_bytes,expires_at)
			VALUES(?,7,11,?,13,?,'MULTIPART','fixture-merged-session ',?,?,?,2,'VERIFYING',?,UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)`,
			taskID, taskID, key+".bin", checksum[:], fileSize, partSize, fileSize)
	}
	execute("UPDATE upload_tasks SET attempt_count=5 WHERE id=?", retryID)
	execute(`INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size,status)
		VALUES(?,7,'local_minio','fixture-bucket','batch-single',?,'VERIFYING')`, singleID, singleSize)
	execute(`INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,
		file_size,status,reserved_bytes,expires_at) VALUES(?,7,11,?,13,'single.bin','SINGLE',?,'VERIFYING',?,UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)`,
		singleID, singleID, singleSize, singleSize)
	readTask := func(taskID uint64) *domain.UploadTask {
		t.Helper()
		task, err := store.FindUserUploadTask(ctx, userID, taskID)
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	singleBefore, deferredBefore := readTask(singleID), readTask(deferredID)

	// 2. 通过正式单轮入口串行处理，首条失配，后两条各自发布和安排重试。
	var opened []uint64
	batch := newServerMultipartUploadVerificationBatch(store, func(_ context.Context, object *domain.StoredObject) (io.ReadCloser, error) {
		opened = append(opened, object.ID)
		switch object.ID {
		case failedID:
			return io.NopCloser(bytes.NewReader(content[:2])), nil
		case publishID:
			return io.NopCloser(bytes.NewReader(content)), nil
		case retryID:
			return nil, domain.ErrUploadStorageUnavailable
		default:
			t.Fatal("批次打开了未选中的对象", object.ID)
			return nil, nil
		}
	})
	const worker = "fixture-multipart-batch-worker"
	for _, invalidLimit := range []int{0, 101} {
		summary, err := batch.RunOnce(ctx, invalidLimit, worker)
		if err == nil || summary.Scanned != 0 || len(opened) != 0 {
			t.Fatal("批次接受了无效上限", invalidLimit, summary, err)
		}
	}
	summary, err := batch.RunOnce(ctx, 3, worker)
	if err != nil || summary.Scanned != 3 || summary.Published != 1 || summary.RetriesScheduled != 1 || len(summary.CandidateErrors) != 1 {
		t.Fatal("Multipart 轮次结果不符", summary, err)
	}
	failure := summary.CandidateErrors[0]
	if failure.TaskID != failedID || !errors.Is(failure.Err, domain.ErrUploadBodySizeMismatch) || !slices.Equal(opened, []uint64{failedID, publishID, retryID}) {
		t.Fatal("单条失败原因或对象读取顺序错误", failure, opened)
	}

	// 3. 回读已提交的任务和配额，核对上限外及 SINGLE 任务原样保留。
	failed, published, retry := readTask(failedID), readTask(publishID), readTask(retryID)
	if failed.Status != domain.UploadStatusFailed || failed.LastErrorCode == nil || *failed.LastErrorCode != domain.UploadBodySizeMismatchCode ||
		failed.LeaseVersion != 1 || failed.LeaseOwner != nil || failed.ReservedBytes != 0 || failed.ResultEntryID != nil {
		t.Fatal("首条失配未提交失败结果", failed)
	}
	if published.Status != domain.UploadStatusCompleted || published.ResultEntryID == nil || published.PublishedObjectID == nil ||
		*published.PublishedObjectID != publishID || published.LeaseVersion != 1 || published.LeaseOwner != nil || published.ReservedBytes != 0 {
		t.Fatal("首条失败后未完成后续发布", published)
	}
	if retry.Status != domain.UploadStatusVerifying || retry.AttemptCount != 6 || retry.LastErrorCode == nil || *retry.LastErrorCode != "STORAGE_UNAVAILABLE" ||
		retry.LeaseVersion != 1 || retry.LeaseOwner != nil || retry.LeaseUntil != nil || retry.ReservedBytes != fileSize ||
		retry.NextAttemptAt == nil || !retry.NextAttemptAt.Equal(retry.UpdatedAt.Add(30*time.Second)) {
		t.Fatal("临时故障未正确登记重试", retry)
	}
	if !reflect.DeepEqual(readTask(singleID), singleBefore) || !reflect.DeepEqual(readTask(deferredID), deferredBefore) {
		t.Fatal("非 Multipart 或上限外的任务被修改")
	}
	var used, reserved uint64
	var publishedFiles int
	if err := pool.QueryRowContext(ctx, `SELECT used_storage,reserved_storage,
		(SELECT COUNT(*) FROM file_entries WHERE user_id=? AND entry_type='FILE') FROM users WHERE id=?`, userID, userID).
		Scan(&used, &reserved, &publishedFiles); err != nil || used != startUsed+fileSize || reserved != 2*fileSize+singleSize || publishedFiles != 1 {
		t.Fatal("轮次提交后的配额或文件数不符", used, reserved, publishedFiles, err)
	}
	t.Logf("顺序 %v，扫描 %d，发布 %d，重试 %d，失败任务 %d，已用 %d，预占 %d",
		opened, summary.Scanned, summary.Published, summary.RetriesScheduled, failure.TaskID, used, reserved)
}
