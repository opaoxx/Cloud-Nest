package repository

import (
	"context"
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

func TestMarkUploadExpirationRetryError(t *testing.T) {
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
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 传入收尾可能返回的错误，核对是否附加标记。
			caller := scenario.caller
			if caller == nil {
				caller = t.Context()
			}
			marked := markUploadExpirationRetryError(caller, scenario.err)
			if errors.Is(marked, domain.ErrUploadExpirationRetryable) != scenario.retryable {
				t.Fatal("重试标记不符", marked)
			}
			// 2. 上层仍可读取原错误，未命中规则的结果原样返回。
			if !errors.Is(marked, scenario.err) || (!scenario.retryable && marked != scenario.err) {
				t.Fatal("原错误未保留", marked)
			}
		})
	}
}

func TestCompleteUploadExpirationMarksLockWaitIntegration(t *testing.T) {
	// 1. 用独占库的一条连接占住暂存对象，另一条连接执行实际收尾。
	pool, store, task := beginUploadExpirationRetryFixture(t)
	pool.SetMaxOpenConns(2)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	holder, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback()
	var lockedID uint64
	if err := holder.QueryRowContext(ctx, "SELECT id FROM stored_objects WHERE id=? FOR UPDATE", task.StagedObjectID).
		Scan(&lockedID); err != nil || lockedID != task.StagedObjectID {
		t.Fatal("夹具未锁住暂存对象", lockedID, err)
	}
	// 持锁事务占用一个连接，只在独占库另一条测试连接上缩短等待。
	if _, err := pool.ExecContext(ctx, "SET SESSION innodb_lock_wait_timeout = 1"); err != nil {
		t.Fatal(err)
	}

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
