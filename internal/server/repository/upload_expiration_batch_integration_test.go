package repository

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestUploadExpirationBatchIntegration(t *testing.T) {
	// 1. 混合单文件和分片候选，分片任务在通用扫描中排第一。
	pool, fixture := uploadTaskDatabase(t)
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
	const multipartID, protectedID, expiredID, deferredID uint64 = 100, 101, 102, 103
	const perTaskReserved, otherReserved, usedBefore = 5, 512, 1024
	var databaseTime time.Time
	if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&databaseTime); err != nil {
		t.Fatal(err)
	}
	expiry := databaseTime.Add(-time.Minute)
	execute("UPDATE users SET reserved_storage=?,used_storage=? WHERE id=?", otherReserved+4*perTaskReserved, usedBefore, fixture.UserID)
	for _, taskID := range []uint64{deferredID, protectedID, multipartID, expiredID} {
		suffix := strconv.FormatUint(taskID, 10)
		execute(`INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size)
			VALUES(?,?,'local_minio','fixture-bucket',?,?)`, taskID, fixture.UserID, "expiry-batch-"+suffix, fixture.FileSize)
		mode := "SINGLE"
		var partSize, partCount any
		if taskID == multipartID {
			mode, partSize, partCount = "MULTIPART", *fixture.PartSize, *fixture.PartCount
		}
		execute(`INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,
			file_size,part_size,part_count,status,reserved_bytes,expires_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,'UPLOADING',?,?)`, taskID, fixture.UserID, fixture.DeviceID, taskID, fixture.TargetParentID,
			"expiry-batch-"+suffix+".bin", mode, fixture.FileSize, partSize, partCount, perTaskReserved, expiry)
	}
	execute("UPDATE file_entries SET object_id=? WHERE id=?", protectedID, *fixture.ResultEntryID)
	readTask := func(taskID uint64) *domain.UploadTask {
		t.Helper()
		task, err := store.FindUserUploadTask(ctx, fixture.UserID, taskID)
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	multipartBefore, deferredBefore := readTask(multipartID), readTask(deferredID)
	unfiltered, err := store.ListUploadExpirationCandidates(ctx, 2)
	if err != nil || len(unfiltered) != 2 || unfiltered[0].TaskID != multipartID || unfiltered[1].TaskID != protectedID {
		t.Fatal("测试夹具在通用扫描中的顺序不符", unfiltered, err)
	}

	// 2. 扫描限量前筛选单文件，再逐条调用实际推进和收尾。
	processor := service.NewUploadExpirationCandidateProcessor(
		store.BeginUploadExpiration, store.CompleteUploadExpiration, store.ScheduleUploadExpirationRetry,
	)
	var processed []uint64
	scanCalls := 0
	batch := service.NewUploadExpirationBatch(func(ctx context.Context, limit int) ([]domain.UploadExpirationCandidate, error) {
		scanCalls++
		return store.ListSingleUploadExpirationCandidates(ctx, limit)
	}, func(ctx context.Context, candidate domain.UploadExpirationCandidate) (*domain.UploadTask, error) {
		processed = append(processed, candidate.TaskID)
		return processor.Process(ctx, candidate)
	})
	summary, err := batch.RunOnce(ctx, 2)
	if err != nil || scanCalls != 1 || summary.Scanned != 2 || summary.Expired != 1 ||
		summary.RetriesScheduled != 0 || len(summary.CandidateErrors) != 1 {
		t.Fatal("到期批次未返回预期汇总", summary, scanCalls, err)
	}
	if !slices.Equal(processed, []uint64{protectedID, expiredID}) || summary.CandidateErrors[0].TaskID != protectedID ||
		!errors.Is(summary.CandidateErrors[0].Err, domain.ErrStoredObjectInUse) {
		t.Fatal("到期批次的候选顺序或错误不符", processed, summary.CandidateErrors)
	}

	// 3. 核对首条失败后的接续状态、成功返还和剩余候选。
	protected, expired := readTask(protectedID), readTask(expiredID)
	if protected.Status != domain.UploadStatusExpiring || protected.ReservedBytes != perTaskReserved ||
		expired.Status != domain.UploadStatusExpired || expired.ReservedBytes != 0 || expired.CompletedAt == nil {
		t.Fatal("批次结果与两个任务的数据库状态不符", protected, expired)
	}
	if !reflect.DeepEqual(multipartBefore, readTask(multipartID)) || !reflect.DeepEqual(deferredBefore, readTask(deferredID)) {
		t.Fatal("分片或上限之外的单文件任务被修改")
	}
	object, err := store.FindUserStoredObject(ctx, fixture.UserID, expiredID)
	if err != nil || object.Status != domain.StoredObjectStatusOrphaned || object.GCAfter == nil {
		t.Fatal("已终结任务的暂存对象未开始计时", object, err)
	}
	var used, reserved uint64
	if err := pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage FROM users WHERE id=?", fixture.UserID).Scan(&used, &reserved); err != nil {
		t.Fatal(err)
	}
	if used != usedBefore || reserved != otherReserved+3*perTaskReserved {
		t.Fatal("批次返还了其他任务的预占或漏返已终结任务", used, reserved)
	}
	remaining, err := store.ListSingleUploadExpirationCandidates(ctx, 100)
	if err != nil || len(remaining) != 2 || remaining[0].TaskID != protectedID || remaining[0].Status != domain.UploadStatusExpiring || remaining[1].TaskID != deferredID {
		t.Fatal("剩余的单文件到期候选错误", remaining, err)
	}
}
