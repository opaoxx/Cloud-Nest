package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestMarkUploadTransactionRetryError(t *testing.T) {
	lockWait := &mysql.MySQLError{Number: 1205}
	deadlock := &mysql.MySQLError{Number: 1213}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()

	for _, scenario := range []struct {
		name      string
		err       error
		caller    context.Context
		retryable bool
	}{
		{name: "success"},
		{name: "lock wait timeout", err: lockWait, retryable: true},
		{name: "wrapped deadlock", err: fmt.Errorf("用户事务: %w", fmt.Errorf("收尾写入: %w", deadlock)), retryable: true},
		{name: "caller canceled", err: deadlock, caller: canceled},
		{name: "wrapped deadline", err: fmt.Errorf("用户事务: %w", context.DeadlineExceeded)},
		{name: "wrapped cancel", err: fmt.Errorf("用户事务: %w", context.Canceled)},
		{name: "failed rollback joined with lock wait", err: fmt.Errorf("用户事务: %w", errors.Join(lockWait, errors.New("回滚失败")))},
		{name: "object still in use", err: domain.ErrStoredObjectInUse},
		{name: "task busy", err: domain.ErrUploadTaskBusy},
		{name: "foreign key violation", err: &mysql.MySQLError{Number: 1452}},
		{name: "bad connection", err: driver.ErrBadConn},
		{name: "matching text only", err: errors.New("ERROR 1205 Lock wait timeout exceeded")},
		{name: "typed nil database error", err: (*mysql.MySQLError)(nil)},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 传入上传事务可能返回的错误，核对是否附加标记。
			caller := scenario.caller
			if caller == nil {
				caller = t.Context()
			}
			for _, marker := range []error{domain.ErrUploadExpirationRetryable, domain.ErrUploadCancellationRetryable, domain.ErrUploadMergeWriteRetryable} {
				marked := markUploadTransactionRetryError(caller, scenario.err, marker)
				if errors.Is(marked, marker) != scenario.retryable {
					t.Fatal("重试标记不符", marker, marked)
				}
				// 2. 上层仍可读取原错误，未命中规则的结果原样返回。
				if !errors.Is(marked, scenario.err) || (!scenario.retryable && marked != scenario.err) {
					t.Fatal("原错误未保留", marked)
				}
			}
		})
	}
}

func TestCompleteUploadExpirationMarksLockWaitIntegration(t *testing.T) {
	// 1. 用独占库的一条连接占住暂存对象，另一条连接执行实际收尾。
	pool, store, task := beginUploadExpirationRetryFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	holder := holdUploadCleanupObjectLock(t, ctx, pool, task.StagedObjectID)

	// 2. 验证真实 MySQL 返回 1205，收尾既保留原原因也附加领域标记。
	result, err := store.CompleteUploadExpiration(ctx, task)
	var databaseError *mysql.MySQLError
	if result != nil || !errors.Is(err, domain.ErrUploadExpirationRetryable) ||
		!errors.As(err, &databaseError) || databaseError.Number != 1205 {
		t.Fatal("收尾未报告预期的锁等待超时", result, err)
	}
	after, err := store.FindUserUploadTask(ctx, task.UserID, task.ID)
	var reserved uint64
	reservationError := pool.QueryRowContext(ctx, "SELECT reserved_storage FROM users WHERE id=?", task.UserID).Scan(&reserved)
	if err != nil || !reflect.DeepEqual(after, task) || reservationError != nil || reserved != 517 {
		t.Fatal("失败的收尾修改了任务或预占", after, reserved, err, reservationError)
	}

	// 3. 保持行锁，用正式批次安排重试。等待中的任务会被下一次扫描略过。
	processor := service.NewUploadExpirationCandidateProcessor(
		store.BeginUploadExpiration, store.CompleteUploadExpiration, store.ScheduleUploadExpirationRetry,
	)
	batch := service.NewUploadExpirationBatch(store.ListSingleUploadExpirationCandidates, processor.Process)
	summary, err := batch.RunOnce(ctx, 1)
	if err != nil || summary.Scanned != 1 || summary.Expired != 0 || summary.RetriesScheduled != 1 || len(summary.CandidateErrors) != 0 {
		t.Fatal("批次未将锁等待超时转为已安排的重试", summary, err)
	}
	scheduled, err := store.FindUserUploadTask(ctx, task.UserID, task.ID)
	if err != nil || scheduled == nil || scheduled.Status != domain.UploadStatusExpiring ||
		scheduled.ReservedBytes != task.ReservedBytes || scheduled.AttemptCount != task.AttemptCount+1 ||
		scheduled.NextAttemptAt == nil || !scheduled.NextAttemptAt.Equal(scheduled.UpdatedAt.Add(16*time.Second)) ||
		scheduled.LastErrorCode == nil || *scheduled.LastErrorCode != "DATABASE_UNAVAILABLE" {
		t.Fatal("临时数据库故障未写回预期的重试安排", scheduled, err)
	}
	if waiting, err := batch.RunOnce(ctx, 1); err != nil || waiting.Scanned != 0 {
		t.Fatal("批次提前处理了等待中的任务", waiting, err)
	}

	// 4. 释放行锁并让等待结束，从批次入口重新扫描和收尾。
	if err := holder.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET next_attempt_at=UTC_TIMESTAMP(6) WHERE id=?", task.ID); err != nil {
		t.Fatal(err)
	}
	summary, err = batch.RunOnce(ctx, 1)
	if err != nil || summary.Scanned != 1 || summary.Expired != 1 || summary.RetriesScheduled != 0 || len(summary.CandidateErrors) != 0 {
		t.Fatal("到点后批次未正常收尾", summary, err)
	}
	finished, err := store.FindUserUploadTask(ctx, task.UserID, task.ID)
	if err != nil || finished == nil || !hasCompletedUploadCleanup(finished, domain.UploadStatusExpired) {
		t.Fatal("锁释放后无法完成收尾", finished, err)
	}
	if err := pool.QueryRowContext(ctx, "SELECT reserved_storage FROM users WHERE id=?", task.UserID).Scan(&reserved); err != nil || reserved != 512 {
		t.Fatal("接续收尾未正确返还任务预占", reserved, err)
	}
}

func TestCompleteUploadCancellationMarksLockWaitIntegration(t *testing.T) {
	// 1. 在独占库准备已受理取消的单文件任务，并占住对应对象行。
	pool, fixture := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if _, err := pool.ExecContext(ctx, "UPDATE users SET reserved_storage=517 WHERE id=?", fixture.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, `UPDATE upload_tasks SET status='CANCELLING',upload_mode='SINGLE',storage_upload_id=NULL,
		part_size=NULL,part_count=NULL,published_object_id=NULL,resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,
		reserved_bytes=5,expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR,lease_owner=NULL,lease_until=NULL,
		attempt_count=4,next_attempt_at=NULL,last_error_code=NULL,last_error_message=NULL
		WHERE id=?`, fixture.ID); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.FindUserUploadTask(ctx, fixture.UserID, fixture.ID)
	if err != nil {
		t.Fatal(err)
	}
	holder := holdUploadCleanupObjectLock(t, ctx, pool, task.StagedObjectID)

	// 2. 确认取消收尾保留真实 MySQL 1205，并附加取消专用标记。
	result, err := store.CompleteUploadCancellation(ctx, task)
	var databaseError *mysql.MySQLError
	if result != nil || !errors.Is(err, domain.ErrUploadCancellationRetryable) || errors.Is(err, domain.ErrUploadExpirationRetryable) ||
		!errors.As(err, &databaseError) || databaseError.Number != 1205 {
		t.Fatal("取消收尾未保留锁等待超时和对应标记", result, err)
	}
	after, err := store.FindUserUploadTask(ctx, task.UserID, task.ID)
	var reserved uint64
	reservationError := pool.QueryRowContext(ctx, "SELECT reserved_storage FROM users WHERE id=?", task.UserID).Scan(&reserved)
	if err != nil || !reflect.DeepEqual(after, task) || reservationError != nil || reserved != 517 {
		t.Fatal("失败的取消收尾修改了任务或预占", after, reserved, err, reservationError)
	}

	// 3. 保持行锁，让真实取消批次安排重试并跳过尚未到点的任务。
	processor := service.NewUploadCancellationCandidateProcessor(
		store.FindUserUploadTask, store.CompleteUploadCancellation, store.ScheduleUploadCancellationRetry,
	)
	batch := service.NewUploadCancellationBatch(store.ListSingleUploadCancellationCandidates, processor.Process)
	summary, err := batch.RunOnce(ctx, 1)
	if err != nil || summary.Scanned != 1 || summary.Cancelled != 0 || summary.RetriesScheduled != 1 || len(summary.CandidateErrors) != 0 {
		t.Fatal("取消批次未将锁等待超时转为重试安排", summary, err)
	}
	scheduled, err := store.FindUserUploadTask(ctx, task.UserID, task.ID)
	if err != nil || scheduled == nil || scheduled.Status != domain.UploadStatusCancelling || scheduled.ReservedBytes != task.ReservedBytes ||
		scheduled.AttemptCount != task.AttemptCount+1 || scheduled.NextAttemptAt == nil ||
		!scheduled.NextAttemptAt.Equal(scheduled.UpdatedAt.Add(16*time.Second)) ||
		scheduled.LastErrorCode == nil || *scheduled.LastErrorCode != "DATABASE_UNAVAILABLE" {
		t.Fatal("未保存预期的取消重试安排", scheduled, err)
	}
	if err := pool.QueryRowContext(ctx, "SELECT reserved_storage FROM users WHERE id=?", task.UserID).Scan(&reserved); err != nil || reserved != 517 {
		t.Fatal("写回重试时变更了账户预占", reserved, err)
	}
	if waiting, err := batch.RunOnce(ctx, 1); err != nil || waiting.Scanned != 0 {
		t.Fatal("取消批次提前选中了任务", waiting, err)
	}

	// 4. 释放行锁，让等待到点并通过下一轮完成取消。
	if err := holder.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET next_attempt_at=UTC_TIMESTAMP(6) WHERE id=?", task.ID); err != nil {
		t.Fatal(err)
	}
	summary, err = batch.RunOnce(ctx, 1)
	if err != nil || summary.Scanned != 1 || summary.Cancelled != 1 || summary.RetriesScheduled != 0 || len(summary.CandidateErrors) != 0 {
		t.Fatal("到点后批次未完成取消收尾", summary, err)
	}
	finished, err := store.FindUserUploadTask(ctx, task.UserID, task.ID)
	if err != nil || finished == nil || !hasCompletedUploadCleanup(finished, domain.UploadStatusCancelled) || finished.AttemptCount != scheduled.AttemptCount {
		t.Fatal("锁释放后无法完成取消收尾", finished, err)
	}
	if err := pool.QueryRowContext(ctx, "SELECT reserved_storage FROM users WHERE id=?", task.UserID).Scan(&reserved); err != nil || reserved != 512 {
		t.Fatal("取消收尾未正确返还任务预占", reserved, err)
	}
}

func holdUploadCleanupObjectLock(t *testing.T, ctx context.Context, pool *sql.DB, objectID uint64) *sql.Tx {
	t.Helper()
	// 1. 占用一个连接并锁定对象行，留另一连接给待验证事务。
	pool.SetMaxOpenConns(2)
	holder, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Rollback() })
	var lockedID uint64
	if err := holder.QueryRowContext(ctx, "SELECT id FROM stored_objects WHERE id=? FOR UPDATE", objectID).
		Scan(&lockedID); err != nil || lockedID != objectID {
		t.Fatal("夹具未锁住暂存对象", lockedID, err)
	}

	// 2. 只在另一条独占测试连接上缩短锁等待时间。
	if _, err := pool.ExecContext(ctx, "SET SESSION innodb_lock_wait_timeout = 1"); err != nil {
		t.Fatal(err)
	}
	return holder
}
