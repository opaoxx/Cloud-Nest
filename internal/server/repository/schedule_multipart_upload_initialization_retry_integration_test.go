package repository

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestScheduleMultipartUploadInitializationRetryIntegration(t *testing.T) {
	pool, _ := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	const worker = "fixture-worker"
	const size = 16777217
	execute := func(t *testing.T, statement string) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	scan := func(t *testing.T, count int) []domain.MultipartUploadInitializationCandidate {
		t.Helper()
		items, err := store.ListMultipartUploadInitializationCandidates(ctx, 1)
		if err != nil || len(items) != count {
			t.Fatal("初始化候选数量不符", items, err)
		}
		return items
	}
	claim := func(t *testing.T) *domain.UploadTask {
		t.Helper()
		task, err := store.ClaimMultipartUploadInitializationLease(ctx, scan(t, 1)[0], worker)
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	reset := func(t *testing.T) {
		t.Helper()
		execute(t, `UPDATE upload_tasks SET status='CREATING',storage_upload_id=NULL,published_object_id=NULL,
			resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,file_size=16777217,part_size=16777216,
			part_count=2,reserved_bytes=16777217,lease_owner=NULL,lease_until=NULL,lease_version=3,
			next_attempt_at=NULL,attempt_count=2,last_error_code=NULL,last_error_message=NULL,
			expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 DAY WHERE id=19`)
	}
	execute(t, "UPDATE users SET reserved_storage=16777217,used_storage=16777216 WHERE id=7")
	execute(t, "UPDATE stored_objects SET status='STAGING',file_size=16777217 WHERE id=23")
	type snapshot struct {
		task           *domain.UploadTask
		object         *domain.StoredObject
		reserved, used uint64
	}
	read := func(t *testing.T) snapshot {
		t.Helper()
		task, err := store.FindUserUploadTask(ctx, 7, 19)
		if err != nil {
			t.Fatal(err)
		}
		object, err := store.FindUserStoredObject(ctx, 7, 23)
		if err != nil {
			t.Fatal(err)
		}
		saved := snapshot{task: task, object: object}
		if err := pool.QueryRowContext(ctx, "SELECT reserved_storage,used_storage FROM users WHERE id=7").Scan(&saved.reserved, &saved.used); err != nil {
			t.Fatal(err)
		}
		return saved
	}
	checkRejected := func(t *testing.T, callContext context.Context, claimed *domain.UploadTask, delay int, want error) {
		t.Helper()
		before := read(t)
		result, err := store.ScheduleMultipartUploadInitializationRetry(callContext, claimed, delay)
		if result != nil || err == nil || (want != nil && !errors.Is(err, want)) || !reflect.DeepEqual(before, read(t)) {
			t.Fatalf("错误写回结果不符或状态已改变，预期 %v，得到 %v %v", want, result, err)
		}
	}

	// 1. 验证登记时间、等待期间和重新领取，核对对象与配额。
	t.Run("schedule and reclaim when due", func(t *testing.T) {
		reset(t)
		claimed := claim(t)
		for _, invalidDelay := range []int{0, 31} {
			checkRejected(t, ctx, claimed, invalidDelay, nil)
		}
		cancelled, stop := context.WithCancel(ctx)
		stop()
		checkRejected(t, cancelled, claimed, 30, context.Canceled)
		before := read(t)
		result, err := store.ScheduleMultipartUploadInitializationRetry(ctx, claimed, 30)
		if err != nil || result == nil {
			t.Fatal("安排初始化重试失败", err)
		}
		wantTime := result.UpdatedAt.Add(30 * time.Second)
		wantCode, wantMessage := "STORAGE_UNAVAILABLE", domain.ErrUploadStorageUnavailable.Error()
		before.task.LeaseOwner, before.task.LeaseUntil = nil, nil
		before.task.NextAttemptAt, before.task.UpdatedAt, before.task.AttemptCount = &wantTime, result.UpdatedAt, 3
		before.task.LastErrorCode, before.task.LastErrorMessage = &wantCode, &wantMessage
		if !reflect.DeepEqual(before.task, result) || !reflect.DeepEqual(before, read(t)) {
			t.Fatal("提交结果、对象或配额与预期不符")
		}
		checkRejected(t, ctx, claimed, 30, domain.ErrUploadTaskBusy)
		scan(t, 0)
		candidate := domain.MultipartUploadInitializationCandidate{TaskID: 19, UserID: 7, StagedObjectID: 23, LeaseVersion: claimed.LeaseVersion}
		if result, err := store.ClaimMultipartUploadInitializationLease(ctx, candidate, worker); result != nil || !errors.Is(err, domain.ErrUploadTaskNotReady) {
			t.Fatal("提前领取绕过了等待时间", err)
		}
		execute(t, "UPDATE upload_tasks SET next_attempt_at=UTC_TIMESTAMP(6) WHERE id=19")
		reclaimed := claim(t)
		if reclaimed.LeaseVersion != claimed.LeaseVersion+1 || reclaimed.AttemptCount != 3 {
			t.Fatal("重新领取未推进租约版本或保留重试次数")
		}
		checkRejected(t, ctx, claimed, 30, domain.ErrUploadTaskBusy)
		result, err = store.ScheduleMultipartUploadInitializationRetry(ctx, reclaimed, 1)
		if err != nil || result == nil || result.AttemptCount != 4 || result.NextAttemptAt == nil || !result.NextAttemptAt.Equal(result.UpdatedAt.Add(time.Second)) {
			t.Fatal("新租约无法继续安排重试", err)
		}
	})
	t.Run("delay stops at task expiry", func(t *testing.T) {
		reset(t)
		claimed := claim(t)
		execute(t, "UPDATE upload_tasks SET expires_at=UTC_TIMESTAMP(6)+INTERVAL 15 SECOND WHERE id=19")
		result, err := store.ScheduleMultipartUploadInitializationRetry(ctx, claimed, 30)
		if err != nil || result == nil || result.NextAttemptAt == nil || !result.NextAttemptAt.Equal(result.ExpiresAt) {
			t.Fatal("等待时间超过任务期限", err)
		}
		execute(t, "UPDATE upload_tasks SET next_attempt_at=UTC_TIMESTAMP(6),expires_at=UTC_TIMESTAMP(6) WHERE id=19")
		scan(t, 0)
	})

	// 2. 已推进的任务、过期租约或耗尽的计数都不再登记。
	for _, test := range []struct {
		name, change string
		want         error
	}{
		{"different holder", "UPDATE upload_tasks SET lease_owner='fixture-worker ' WHERE id=19", domain.ErrUploadTaskBusy},
		{"expired lease", "UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6) WHERE id=19", domain.ErrUploadTaskBusy},
		{"expired task", "UPDATE upload_tasks SET expires_at=UTC_TIMESTAMP(6) WHERE id=19", domain.ErrUploadTaskBusy},
		{"session bound", "UPDATE upload_tasks SET status='UPLOADING',storage_upload_id='fixture-session' WHERE id=19", domain.ErrUploadTaskNotReady},
		{"reservation changed", "UPDATE upload_tasks SET reserved_bytes=file_size-1 WHERE id=19", domain.ErrInvalidUploadTaskData},
		{"attempt count exhausted", "UPDATE upload_tasks SET attempt_count=4294967295 WHERE id=19", domain.ErrInvalidUploadTaskData},
	} {
		t.Run(test.name, func(t *testing.T) {
			reset(t)
			claimed := claim(t)
			execute(t, test.change)
			checkRejected(t, ctx, claimed, 30, test.want)
		})
	}
	t.Run("lease version exhausted", func(t *testing.T) {
		reset(t)
		execute(t, "UPDATE upload_tasks SET lease_version=18446744073709551614 WHERE id=19")
		checkRejected(t, ctx, claim(t), 30, domain.ErrUploadLeaseVersionExhausted)
	})
	t.Run("invalid readback rolls back", func(t *testing.T) {
		reset(t)
		claimed := claim(t)
		execute(t, "CREATE TRIGGER fixture_retry BEFORE UPDATE ON upload_tasks FOR EACH ROW SET NEW.attempt_count=OLD.attempt_count")
		t.Cleanup(func() { execute(t, "DROP TRIGGER fixture_retry") })
		checkRejected(t, ctx, claimed, 30, domain.ErrInvalidUploadTaskData)
	})
}
