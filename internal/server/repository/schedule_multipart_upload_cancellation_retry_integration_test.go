package repository

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
)

func TestScheduleMultipartUploadCancellationRetryIntegration(t *testing.T) {
	ctx, pool, store := prepareMultipartCompletionDatabase(t)
	pool.SetMaxOpenConns(1)
	execute := func(t *testing.T, statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	defer execute(t, "SET timestamp = DEFAULT")
	start := time.Now().UTC().Truncate(time.Second)
	clock := func(t *testing.T, elapsed int) {
		t.Helper()
		execute(t, "SET timestamp = ?", start.Unix()+int64(elapsed))
	}
	scan := func(t *testing.T, count int) []domain.UploadCancellationCandidate {
		t.Helper()
		candidates, err := store.ListMultipartUploadCancellationCandidates(ctx, 1)
		if err != nil || len(candidates) != count {
			t.Fatal("取消候选数量不符", candidates, err)
		}
		return candidates
	}
	claim := func(t *testing.T, beforeClaim ...string) *domain.UploadTask {
		t.Helper()
		clock(t, 0)
		resetMultipartUploadCancellationFixture(t, ctx, pool)
		execute(t, "UPDATE upload_tasks SET attempt_count=2,last_error_code=NULL,last_error_message=NULL WHERE id=19")
		for _, statement := range beforeClaim {
			if statement != "" {
				execute(t, statement)
			}
		}
		claimed, err := store.ClaimMultipartUploadCancellationLease(ctx, scan(t, 1)[0], "fixture-cancel-retry")
		if err != nil || claimed == nil || claimed.LeaseUntil == nil || !claimed.LeaseUntil.Equal(start.Add(time.Minute)) {
			t.Fatal("未取得预期的取消租约", err)
		}
		return claimed
	}
	type facts struct {
		task           *domain.UploadTask
		object         *domain.StoredObject
		used, reserved uint64
	}
	read := func(t *testing.T) facts {
		t.Helper()
		task, err := store.FindUserUploadTask(ctx, 7, 19)
		if err != nil {
			t.Fatal(err)
		}
		object, err := store.FindUserStoredObject(ctx, 7, 23)
		if err != nil {
			t.Fatal(err)
		}
		current := facts{task: task, object: object}
		if err := pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage FROM users WHERE id=7").Scan(&current.used, &current.reserved); err != nil {
			t.Fatal(err)
		}
		return current
	}

	// 1. 用数据库时钟验证两种原因、原租约下限及到点重新领取。
	for _, fixture := range []struct {
		name                 string
		beforeClaim          []string
		delay, elapsed, next int
		reason               domain.MultipartUploadCancellationRetryReason
		message              string
	}{
		{name: "storage retry waits for original lease", delay: 1, next: 60,
			reason: domain.MultipartCancellationRetryStorage, message: domain.ErrUploadStorageUnavailable.Error()},
		{name: "database retry respects later backoff for unbound disabled account",
			beforeClaim: []string{"UPDATE users SET status='DISABLED' WHERE id=7", "UPDATE upload_tasks SET storage_upload_id=NULL WHERE id=19"},
			delay:       30, elapsed: 50, next: 80, reason: domain.MultipartCancellationRetryDatabase, message: "数据服务暂时不可用"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			claimed := claim(t, fixture.beforeClaim...)
			if fixture.reason == domain.MultipartCancellationRetryDatabase && claimed.StorageUploadID != nil {
				t.Fatal("未取得未绑定会话的合成租约")
			}
			clock(t, fixture.elapsed)
			before := read(t)
			updated, err := store.ScheduleMultipartUploadCancellationRetry(ctx, claimed, fixture.delay, fixture.reason)
			if err != nil || updated == nil {
				t.Fatal("登记取消重试失败", err)
			}
			before.task.LeaseOwner, before.task.LeaseUntil = nil, nil
			before.task.AttemptCount++
			before.task.NextAttemptAt = new(start.Add(time.Duration(fixture.next) * time.Second))
			before.task.UpdatedAt = start.Add(time.Duration(fixture.elapsed) * time.Second)
			before.task.LastErrorCode, before.task.LastErrorMessage = new(string(fixture.reason)), new(fixture.message)
			if !reflect.DeepEqual(before.task, updated) || !reflect.DeepEqual(before, read(t)) {
				t.Fatal("未准确保存重试安排、原绑定及预占")
			}
			if result, err := store.ScheduleMultipartUploadCancellationRetry(ctx, claimed, fixture.delay, fixture.reason); result != nil ||
				!errors.Is(err, domain.ErrUploadTaskBusy) || !reflect.DeepEqual(before, read(t)) {
				t.Fatal("同一租约重复登记了重试", err)
			}
			clock(t, fixture.next-1)
			scan(t, 0)
			clock(t, fixture.next)
			next, err := store.ClaimMultipartUploadCancellationLease(ctx, scan(t, 1)[0], "fixture-next-cancel")
			if err != nil || next == nil || next.LeaseVersion != claimed.LeaseVersion+1 || next.AttemptCount != updated.AttemptCount ||
				!sameUploadStorageText(next.StorageUploadID, claimed.StorageUploadID) || !next.ExpiresAt.Before(start) {
				t.Fatal("重试到点后未能领取原已到期任务的新租约", err)
			}
			current := read(t)
			if result, err := store.ScheduleMultipartUploadCancellationRetry(ctx, claimed, fixture.delay, fixture.reason); result != nil ||
				!errors.Is(err, domain.ErrUploadTaskBusy) || !reflect.DeepEqual(current, read(t)) {
				t.Fatal("新租约出现后旧执行者仍能改写重试", err)
			}
			t.Logf("原租约第 60 秒截止，%s 第 %d 秒重领版本 %d，已用 %d B、预占 %d B",
				fixture.reason, fixture.next, next.LeaseVersion, current.used, current.reserved)
		})
	}

	// 2. 在读取当前任务后推进时钟，证明实际 UPDATE 也遵守租约边界。
	t.Run("lease expires after locked lookup", func(t *testing.T) {
		claimed := claim(t)
		before := read(t)
		advanced := false
		const callback = "fixture:expire_cancellation_retry_after_lookup"
		queries := store.database.Callback().Query()
		if err := queries.After("gorm:query").Register(callback, func(query *gorm.DB) {
			if query.Error == nil && query.Statement.Table == "upload_tasks" && !advanced {
				advanced = true
				_, err := query.Statement.ConnPool.ExecContext(query.Statement.Context, "SET timestamp = ?", claimed.LeaseUntil.Unix())
				query.AddError(err)
			}
		}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := queries.Remove(callback); err != nil {
				t.Error(err)
			}
		})
		if result, err := store.ScheduleMultipartUploadCancellationRetry(ctx, claimed, 1, domain.MultipartCancellationRetryStorage); result != nil ||
			!advanced || !errors.Is(err, domain.ErrUploadTaskBusy) || !reflect.DeepEqual(before, read(t)) {
			t.Fatal("写入未拦截恰好到期的租约，或留下变动", err)
		}
		t.Log("锁内读任务后推进时钟到租约截止，重试更新返回 UPLOAD_BUSY")
	})

	// 3. 验证过时输入、计数边界或写回异常均不能改动待取消任务。
	for _, fixture := range []struct {
		name, beforeClaim, change, trigger string
		edit                               func(*domain.UploadTask)
		want                               error
	}{
		{name: "lease version changed", change: "UPDATE upload_tasks SET lease_version=lease_version+1 WHERE id=19", want: domain.ErrUploadTaskBusy},
		{name: "lease owner changed", change: "UPDATE upload_tasks SET lease_owner='other-worker' WHERE id=19", want: domain.ErrUploadTaskBusy},
		{name: "attempt count changed", change: "UPDATE upload_tasks SET attempt_count=attempt_count+1 WHERE id=19", want: domain.ErrUploadTaskBusy},
		{name: "raw session changed", change: "UPDATE upload_tasks SET storage_upload_id=RTRIM(storage_upload_id) WHERE id=19", want: domain.ErrUploadTaskBusy},
		{name: "object binding changed", change: "UPDATE upload_tasks SET staged_object_id=29 WHERE id=19", want: domain.ErrUploadTaskBusy},
		{name: "retry already deferred", change: "UPDATE upload_tasks SET next_attempt_at=UTC_TIMESTAMP(6)+INTERVAL 1 MINUTE WHERE id=19", want: domain.ErrUploadTaskBusy},
		{name: "task state changed", change: "UPDATE upload_tasks SET status='EXPIRING' WHERE id=19", want: domain.ErrUploadTaskNotReady},
		{name: "wrong account", edit: func(task *domain.UploadTask) { task.UserID = 8 }, want: domain.ErrUploadTaskNotFound},
		{name: "attempt count exhausted", beforeClaim: "UPDATE upload_tasks SET attempt_count=4294967295 WHERE id=19", want: domain.ErrInvalidUploadTaskData},
		{name: "future lease version exhausted", beforeClaim: "UPDATE upload_tasks SET lease_version=18446744073709551614 WHERE id=19", want: domain.ErrUploadLeaseVersionExhausted},
		{name: "readback rejects incorrect time", trigger: "fixture_cancel_retry", change: "CREATE TRIGGER fixture_cancel_retry BEFORE UPDATE ON upload_tasks FOR EACH ROW SET NEW.next_attempt_at=NEW.updated_at", want: domain.ErrInvalidUploadTaskData},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			claimed := claim(t, fixture.beforeClaim)
			if fixture.change != "" {
				execute(t, fixture.change)
			}
			if fixture.trigger != "" {
				defer execute(t, "DROP TRIGGER IF EXISTS "+fixture.trigger)
			}
			if fixture.edit != nil {
				fixture.edit(claimed)
			}
			before := read(t)
			if result, err := store.ScheduleMultipartUploadCancellationRetry(ctx, claimed, 1, domain.MultipartCancellationRetryStorage); result != nil ||
				!errors.Is(err, fixture.want) || !reflect.DeepEqual(before, read(t)) {
				t.Fatal("资格校验或回滚结果不符", fixture.want, err)
			}
		})
	}

	t.Run("invalid reason waits or canceled input", func(t *testing.T) {
		claimed := claim(t)
		before := read(t)
		for _, delay := range []int{0, 31, math.MaxInt} {
			if result, err := store.ScheduleMultipartUploadCancellationRetry(ctx, claimed, delay, domain.MultipartCancellationRetryStorage); result != nil || err == nil {
				t.Fatal("无效等待时间被接受", delay, err)
			}
		}
		if result, err := store.ScheduleMultipartUploadCancellationRetry(ctx, claimed, 1, "raw-error"); result != nil || !errors.Is(err, domain.ErrInvalidUploadTaskData) {
			t.Fatal("无效重试原因被接受", err)
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if result, err := store.ScheduleMultipartUploadCancellationRetry(canceled, claimed, 1, domain.MultipartCancellationRetryStorage); result != nil ||
			!errors.Is(err, context.Canceled) || !reflect.DeepEqual(before, read(t)) {
			t.Fatal("上下文取消没有保留或出现数据变化", err)
		}
	})
}
