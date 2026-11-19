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

func TestUploadCancellationBatchIntegration(t *testing.T) {
	// 1. 准备排序靠前的分片任务、仍被引用的对象与可正常收尾的任务。
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
	const multipartID, protectedID, cancelledID, deferredID uint64 = 100, 101, 102, 103
	const perTaskReserved, otherReserved, usedBefore = 5, 512, 1024
	execute("UPDATE users SET reserved_storage=?,used_storage=? WHERE id=?", otherReserved+4*perTaskReserved, usedBefore, fixture.UserID)
	for _, taskID := range []uint64{deferredID, protectedID, multipartID, cancelledID} {
		suffix := strconv.FormatUint(taskID, 10)
		execute(`INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size)
			VALUES(?,?,'local_minio','fixture-bucket',?,?)`, taskID, fixture.UserID, "cancel-batch-"+suffix, fixture.FileSize)
		mode := "SINGLE"
		var partSize, partCount any
		if taskID == multipartID {
			mode, partSize, partCount = "MULTIPART", *fixture.PartSize, *fixture.PartCount
		}
		execute(`INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,
			file_size,part_size,part_count,status,reserved_bytes,expires_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,'CANCELLING',?,UTC_TIMESTAMP(6)+INTERVAL 1 DAY)`, taskID, fixture.UserID, fixture.DeviceID,
			taskID, fixture.TargetParentID, "cancel-batch-"+suffix+".bin", mode, fixture.FileSize, partSize, partCount, perTaskReserved)
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
	multipartBefore, deferredBefore, protectedBefore := readTask(multipartID), readTask(deferredID), readTask(protectedID)

	// 2. 通过循环执行真实扫描和单条处理，首轮结果交付后结束。
	processor := service.NewUploadCancellationCandidateProcessor(
		store.FindUserUploadTask, store.CompleteUploadCancellation, store.ScheduleUploadCancellationRetry,
	)
	scanCalls := 0
	var processed []uint64
	batch := service.NewUploadCancellationBatch(func(ctx context.Context, limit int) ([]domain.UploadCancellationCandidate, error) {
		scanCalls++
		return store.ListSingleUploadCancellationCandidates(ctx, limit)
	}, func(ctx context.Context, candidate domain.UploadCancellationCandidate) (*domain.UploadTask, error) {
		processed = append(processed, candidate.TaskID)
		return processor.Process(ctx, candidate)
	})
	loopContext, stopLoop := context.WithCancel(ctx)
	defer stopLoop()
	var summary service.UploadCancellationBatchResult
	var roundError error
	reports := 0
	err = service.RunUploadCancellationLoop(loopContext, 30*time.Second, func(roundContext context.Context) (service.UploadCancellationBatchResult, error) {
		return batch.RunOnce(roundContext, 2)
	}, func(result service.UploadCancellationBatchResult, resultError error) {
		summary, roundError = result, resultError
		reports++
		stopLoop()
	})
	if !errors.Is(err, context.Canceled) || roundError != nil || reports != 1 || scanCalls != 1 ||
		summary.Scanned != 2 || summary.Cancelled != 1 || len(summary.CandidateErrors) != 1 {
		t.Fatal("一轮取消收尾的扫描、汇报或结果总数不符", summary, scanCalls, reports, roundError, err)
	}
	if !slices.Equal(processed, []uint64{protectedID, cancelledID}) || summary.CandidateErrors[0].TaskID != protectedID ||
		!errors.Is(summary.CandidateErrors[0].Err, domain.ErrStoredObjectInUse) {
		t.Fatal("候选顺序或单条错误不符", processed, summary.CandidateErrors)
	}

	// 3. 验证只有成功候选返还预占并开始对象回收计时。
	finished := readTask(cancelledID)
	if !hasCompletedUploadCleanup(finished, domain.UploadStatusCancelled) {
		t.Fatal("成功候选未写入完整取消终态", finished)
	}
	if !reflect.DeepEqual(multipartBefore, readTask(multipartID)) || !reflect.DeepEqual(deferredBefore, readTask(deferredID)) ||
		!reflect.DeepEqual(protectedBefore, readTask(protectedID)) {
		t.Fatal("失败、分片或上限之外的任务发生变化")
	}
	object, err := store.FindUserStoredObject(ctx, fixture.UserID, cancelledID)
	if err != nil || object.Status != domain.StoredObjectStatusOrphaned || object.GCAfter == nil {
		t.Fatal("取消成功对象未开始回收计时", object, err)
	}
	var used, reserved uint64
	if err := pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage FROM users WHERE id=?", fixture.UserID).Scan(&used, &reserved); err != nil {
		t.Fatal(err)
	}
	if used != usedBefore || reserved != otherReserved+3*perTaskReserved {
		t.Fatal("账户预占未按已终结任务的实际字节数返还", used, reserved)
	}
	remaining, err := store.ListSingleUploadCancellationCandidates(ctx, 100)
	if err != nil || len(remaining) != 2 || remaining[0].TaskID != protectedID || remaining[1].TaskID != deferredID {
		t.Fatal("一轮过后的待取消候选不符", remaining, err)
	}
}
