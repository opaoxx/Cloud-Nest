package repository

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestScheduleUploadExpirationRetryIntegration(t *testing.T) {
	// 1. 取得已推进的任务，用独占连接的 MySQL 时钟复验等待边界。
	ctx := t.Context()
	pool, store, task := beginUploadExpirationRetryFixture(t)
	pool.SetMaxOpenConns(1)
	now := task.UpdatedAt.Truncate(time.Second).Add(time.Second)
	nextAttempt := now.Add(30 * time.Second)
	setClock := func(stamp time.Time) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, "SET timestamp = ?", stamp.Unix()); err != nil {
			t.Fatal(err)
		}
	}
	setClock(now)
	t.Cleanup(func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := pool.ExecContext(cleanupContext, "SET timestamp = DEFAULT"); err != nil {
			t.Error("恢复测试连接时钟失败", err)
		}
	})
	list := func() []domain.UploadExpirationCandidate {
		t.Helper()
		candidates, err := store.ListSingleUploadExpirationCandidates(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		return candidates
	}
	originalCandidates := list()
	if len(originalCandidates) != 1 || originalCandidates[0].TaskID != task.ID {
		t.Fatal("等待写回前的过期候选错误", originalCandidates)
	}
	objectBefore, err := store.FindUserStoredObject(ctx, task.UserID, task.StagedObjectID)
	if err != nil {
		t.Fatal(err)
	}

	// 2. 写回并核对提交结果，重复、过早及取消请求不得改变它。
	for _, delay := range []int{0, 31} {
		if result, err := store.ScheduleUploadExpirationRetry(ctx, task, delay); result != nil || err == nil {
			t.Fatal("接受了超出范围的等待时间", delay, result, err)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if result, err := store.ScheduleUploadExpirationRetry(canceled, task, 30); result != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("未保留取消错误", result, err)
	}
	scheduled, err := store.ScheduleUploadExpirationRetry(ctx, task, 30)
	if err != nil {
		t.Fatal(err)
	}
	want := *task
	code, message := "DATABASE_UNAVAILABLE", "数据服务暂时不可用"
	want.NextAttemptAt, want.UpdatedAt, want.AttemptCount = &nextAttempt, now, task.AttemptCount+1
	want.LastErrorCode, want.LastErrorMessage = &code, &message
	actual, err := store.FindUserUploadTask(ctx, task.UserID, task.ID)
	if err != nil || !reflect.DeepEqual(scheduled, actual) || !reflect.DeepEqual(&want, actual) {
		t.Fatal("写回后的任务与预期不符", actual, scheduled, want, err)
	}
	objectAfter, err := store.FindUserStoredObject(ctx, task.UserID, task.StagedObjectID)
	if err != nil || !reflect.DeepEqual(objectBefore, objectAfter) {
		t.Fatal("写回修改了暂存对象", objectAfter, err)
	}
	var reserved uint64
	if err := pool.QueryRowContext(ctx, "SELECT reserved_storage FROM users WHERE id=?", task.UserID).Scan(&reserved); err != nil || reserved != 517 {
		t.Fatal("写回修改了账户预占", reserved, err)
	}
	if pending, err := store.ScheduleUploadExpirationRetry(ctx, scheduled, 1); pending != nil || !errors.Is(err, domain.ErrUploadTaskBusy) {
		t.Fatal("尚未到点的安排被改写", pending, err)
	}
	if hidden := list(); len(hidden) != 0 {
		t.Fatal("扫描提前取到了过期候选", hidden)
	}
	if pending, err := store.CompleteUploadExpiration(ctx, scheduled); pending != nil || !errors.Is(err, domain.ErrUploadTaskBusy) {
		t.Fatal("收尾绕过了等待时间", pending, err)
	}

	// 3. 到点后扫描可接续，旧快照仍不能重复写回。
	setClock(nextAttempt)
	ready := list()
	if !reflect.DeepEqual(originalCandidates, ready) {
		t.Fatal("到点后没有恢复同一候选", ready)
	}
	if repeated, err := store.ScheduleUploadExpirationRetry(ctx, task, 30); repeated != nil || !errors.Is(err, domain.ErrUploadTaskBusy) {
		t.Fatal("旧快照在到点后重复推迟了任务", repeated, err)
	}
	reopened, err := store.BeginUploadExpiration(ctx, ready[0])
	if err != nil || !reflect.DeepEqual(reopened, scheduled) {
		t.Fatal("到点后未恢复已提交的任务", reopened, err)
	}
	again, err := store.ScheduleUploadExpirationRetry(ctx, reopened, 1)
	if err != nil || again == nil || again.AttemptCount != scheduled.AttemptCount+1 || again.NextAttemptAt == nil ||
		!again.NextAttemptAt.Equal(nextAttempt.Add(time.Second)) {
		t.Fatal("新的任务快照未能安排下一次处理", again, err)
	}
	setClock(*again.NextAttemptAt)
	finished, err := store.CompleteUploadExpiration(ctx, again)
	if err != nil || finished == nil || !hasCompletedUploadCleanup(finished, domain.UploadStatusExpired) || finished.AttemptCount != again.AttemptCount {
		t.Fatal("收尾未能在到点后提交，并清理重试时间和诊断", finished, err)
	}
}

func TestScheduleUploadExpirationRetryRejectsConflictsIntegration(t *testing.T) {
	for _, scenario := range []struct {
		name, changeSQL string
		wantError       error
	}{
		{"active lease", "UPDATE upload_tasks SET lease_owner='other',lease_until=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR WHERE id=19", domain.ErrUploadTaskBusy},
		{"before original deadline", "UPDATE upload_tasks SET expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR WHERE id=19", domain.ErrUploadTaskBusy},
		{"competing cancellation", "UPDATE upload_tasks SET status='CANCELLING' WHERE id=19", domain.ErrUploadTaskNotReady},
		{"attempt count exhausted", "UPDATE upload_tasks SET attempt_count=4294967295 WHERE id=19", domain.ErrInvalidUploadTaskData},
		{"invalid readback rolls back", "CREATE TRIGGER fixture_expiry_retry BEFORE UPDATE ON upload_tasks FOR EACH ROW SET NEW.last_error_code='fixture'", domain.ErrInvalidUploadTaskData},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 制造条件冲突或读回异常，保留写回前的完整任务。
			ctx := t.Context()
			pool, store, task := beginUploadExpirationRetryFixture(t)
			if _, err := pool.ExecContext(ctx, scenario.changeSQL); err != nil {
				t.Fatal(err)
			}
			before, err := store.FindUserUploadTask(ctx, task.UserID, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			task.AttemptCount = before.AttemptCount
			task.ExpiresAt = before.ExpiresAt

			// 2. 核对错误及事务失败后保存的任务。
			got, err := store.ScheduleUploadExpirationRetry(ctx, task, 10)
			if got != nil || !errors.Is(err, scenario.wantError) {
				t.Fatal("冲突未被拒绝", got, err)
			}
			after, err := store.FindUserUploadTask(ctx, task.UserID, task.ID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("被拒绝的事务改变了任务", after, err)
			}
		})
	}
}

func beginUploadExpirationRetryFixture(t *testing.T) (*sql.DB, *Store, *domain.UploadTask) {
	t.Helper()
	// 1. 在独占库准备有部分预占的已到期单文件任务。
	pool, fixture := uploadTaskDatabase(t)
	ctx := t.Context()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE users SET status='DISABLED',reserved_storage=517 WHERE id=?", fixture.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, `UPDATE upload_tasks SET status='UPLOADING',upload_mode='SINGLE',
		storage_upload_id=NULL,part_size=NULL,part_count=NULL,published_object_id=NULL,resolved_name=NULL,
		result_entry_id=NULL,completed_at=NULL,reserved_bytes=5,expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 MINUTE,
		lease_owner=NULL,lease_until=NULL,lease_version=?,attempt_count=4 WHERE id=?`,
		uint64(math.MaxUint64), fixture.ID); err != nil {
		t.Fatal(err)
	}
	// 2. 通过正式扫描和推进得到写回事务需要的任务快照。
	candidates, err := store.ListSingleUploadExpirationCandidates(ctx, 1)
	if err != nil || len(candidates) != 1 {
		t.Fatal("未找到待过期任务", candidates, err)
	}
	task, err := store.BeginUploadExpiration(ctx, candidates[0])
	if err != nil {
		t.Fatal(err)
	}
	return pool, store, task
}
