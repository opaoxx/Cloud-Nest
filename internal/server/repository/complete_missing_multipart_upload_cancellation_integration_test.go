package repository

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
)

func TestCompleteMissingMultipartUploadCancellationIntegration(t *testing.T) {
	ctx, pool, store := prepareMultipartCompletionDatabase(t)
	const provider, bucket, key = "local_minio", "fixture-bucket", "fixture/missing +%.bin"
	execute := func(t *testing.T, statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	claim := func(t *testing.T, unbound bool) *domain.MultipartUploadCancellationObservation {
		t.Helper()
		resetMultipartUploadCancellationFixture(t, ctx, pool)
		execute(t, "UPDATE file_entries SET object_id=29 WHERE id=31")
		execute(t, "UPDATE stored_objects SET provider=?,bucket=?,object_key=? WHERE id=23", provider, bucket, key)
		execute(t, `UPDATE upload_tasks SET next_attempt_at=UTC_TIMESTAMP(6),attempt_count=3,
			last_error_code='STORAGE_UNAVAILABLE',last_error_message='fixture previous retry' WHERE id=19`)
		if unbound {
			execute(t, "UPDATE upload_tasks SET storage_upload_id=NULL WHERE id=19")
			execute(t, "UPDATE users SET status='DISABLED' WHERE id=7")
		}
		candidates, err := store.ListMultipartUploadCancellationCandidates(ctx, 1)
		if err != nil || len(candidates) != 1 || candidates[0].TaskID != 19 {
			t.Fatal("未取得取消候选", candidates, err)
		}
		task, err := store.ClaimMultipartUploadCancellationLease(ctx, candidates[0], "fixture-cancel-terminal")
		if err != nil {
			t.Fatal("未领取取消租约", err)
		}
		return &domain.MultipartUploadCancellationObservation{Task: *task, ObjectKey: key}
	}
	type facts struct {
		task           *domain.UploadTask
		object         *domain.StoredObject
		used, reserved uint64
		accountUpdated time.Time
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
		result := facts{task: task, object: object}
		if err := pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage,updated_at FROM users WHERE id=7").
			Scan(&result.used, &result.reserved, &result.accountUpdated); err != nil {
			t.Fatal(err)
		}
		return result
	}
	complete := func(observation *domain.MultipartUploadCancellationObservation) (*domain.UploadTask, error) {
		return store.CompleteMissingMultipartUploadCancellation(ctx, provider, bucket, observation)
	}
	checkCommitted := func(t *testing.T, original facts, result *domain.UploadTask) facts {
		t.Helper()
		actual := read(t)
		if result == nil || !reflect.DeepEqual(actual.task, result) || result.CompletedAt == nil ||
			result.CompletedAt.Before(original.task.ExpiresAt) || !result.CompletedAt.Before(*original.task.LeaseUntil) {
			t.Fatal("提交结果与库内时间不符", result)
		}
		expectedTask := *original.task
		expectedTask.Status, expectedTask.ReservedBytes = domain.UploadStatusCancelled, 0
		expectedTask.CompletedAt, expectedTask.UpdatedAt = result.CompletedAt, *result.CompletedAt
		expectedTask.LeaseOwner, expectedTask.LeaseUntil, expectedTask.NextAttemptAt = nil, nil, nil
		expectedTask.LastErrorCode, expectedTask.LastErrorMessage = nil, nil
		if !reflect.DeepEqual(&expectedTask, result) {
			t.Fatal("终态未保留原计划、会话及历史字段")
		}
		if actual.used != original.used || actual.reserved != original.reserved-original.task.ReservedBytes ||
			!actual.accountUpdated.Equal(*result.CompletedAt) {
			t.Fatal("账号账目没有按本任务预占结算", actual.used, actual.reserved)
		}
		orphaned := actual.object
		if orphaned.UnreferencedAt == nil || orphaned.GCAfter == nil || orphaned.UnreferencedAt.Before(*result.CompletedAt) ||
			!orphaned.GCAfter.Equal(orphaned.UnreferencedAt.Add(7*24*time.Hour)) {
			t.Fatal("未登记准确的七天回收宽限")
		}
		expectedObject := *original.object
		expectedObject.Status, expectedObject.UnreferencedAt, expectedObject.GCAfter = domain.StoredObjectStatusOrphaned, orphaned.UnreferencedAt, orphaned.GCAfter
		expectedObject.UpdatedAt = *orphaned.UnreferencedAt
		if !reflect.DeepEqual(&expectedObject, orphaned) {
			t.Fatal("回收登记修改了额外的对象字段")
		}
		return actual
	}

	// 1. 两个执行请求同时交付同一观察，仅提交一次结算并复用结果。
	t.Run("concurrent completion uses a single settlement", func(t *testing.T) {
		observation := claim(t, false)
		before := read(t)
		type outcome struct {
			task *domain.UploadTask
			err  error
		}
		start, outcomes := make(chan struct{}), make(chan outcome, 2)
		for range 2 {
			go func() {
				<-start
				result, err := complete(observation)
				outcomes <- outcome{task: result, err: err}
			}()
		}
		close(start)
		first, second := <-outcomes, <-outcomes
		if first.err != nil || second.err != nil || !reflect.DeepEqual(first.task, second.task) {
			t.Fatal("并发交付未返回同一终态", first.err, second.err)
		}
		actual := checkCommitted(t, before, first.task)
		t.Logf("两个调用返回同一 CANCELLED，已用 %d B，预占 %d B，回收宽限 7 天", actual.used, actual.reserved)
	})

	t.Run("unbound disabled account and replay after lease expiry", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		now := time.Now().UTC().Truncate(time.Second)
		execute(t, "SET timestamp = ?", now.Unix())
		defer execute(t, "SET timestamp = DEFAULT")
		observation := claim(t, true)
		before := read(t)
		result, err := complete(observation)
		if err != nil {
			t.Fatal(err)
		}
		checkCommitted(t, before, result)
		execute(t, "SET timestamp = ?", observation.Task.LeaseUntil.Add(8*24*time.Hour).Unix())
		execute(t, "UPDATE stored_objects SET status='DELETED',updated_at=UTC_TIMESTAMP(6) WHERE id=23")
		replayBefore := read(t)
		replayed, err := complete(observation)
		if err != nil || !reflect.DeepEqual(result, replayed) || !reflect.DeepEqual(replayBefore, read(t)) {
			t.Fatal("重试未复用已提交终态或再次写入", err)
		}
		stale := *observation
		stale.Task.LeaseVersion--
		if result, err := complete(&stale); result != nil || !errors.Is(err, domain.ErrUploadTaskBusy) {
			t.Fatal("其他租约版本复用了本次结果", err)
		}
	})

	// 2. 锁内复核通过后推进时钟，确认 UPDATE 也会拒绝恰好到期的租约。
	t.Run("lease expires before conditional update", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		now := time.Now().UTC().Truncate(time.Second)
		execute(t, "SET timestamp = ?", now.Unix())
		defer execute(t, "SET timestamp = DEFAULT")
		observation := claim(t, false)
		before := read(t)
		reads := 0
		const callback = "fixture:expire_multipart_cancellation_at_quota_read"
		queries := store.database.Callback().Query()
		if err := queries.After("gorm:query").Register(callback, func(query *gorm.DB) {
			if query.Error != nil || query.Statement.Table != "users" {
				return
			}
			reads++
			if reads == 2 {
				_, err := query.Statement.ConnPool.ExecContext(query.Statement.Context, "SET timestamp = ?", observation.Task.LeaseUntil.Unix())
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
		if result, err := complete(observation); result != nil || !errors.Is(err, domain.ErrUploadTaskBusy) ||
			reads != 2 || !reflect.DeepEqual(read(t), before) {
			t.Fatal("条件更新未拦截恰好到期的租约，或留下写入", reads, err)
		}
		t.Log("读取配额后将数据库时钟推进至租约截止，UPDATE 返回 UPLOAD_BUSY")
	})

	// 3. 资格不符或收尾报错时，三个记录整体保留原值。
	for _, fixture := range []struct {
		name, change, trigger string
		withObject            bool
		want                  error
	}{
		{name: "object exists in observation", withObject: true, want: domain.ErrUploadTaskNotReady},
		{name: "session changed", change: "UPDATE upload_tasks SET storage_upload_id=RTRIM(storage_upload_id) WHERE id=19", want: domain.ErrUploadTaskBusy},
		{name: "account reservation insufficient", change: "UPDATE users SET reserved_storage=1 WHERE id=7", want: domain.ErrInvalidAccountData},
		{name: "recorded object contradicts missing", change: "UPDATE stored_objects SET status='VERIFYING',storage_etag='fixture',storage_version_id='version' WHERE id=23", want: domain.ErrInvalidStoredObjectData},
		{name: "object still referenced", change: "UPDATE file_entries SET object_id=23 WHERE id=31", want: domain.ErrStoredObjectInUse},
		{name: "rollback on invalid task readback", trigger: "fixture_cancel_task", change: "CREATE TRIGGER fixture_cancel_task BEFORE UPDATE ON upload_tasks FOR EACH ROW SET NEW.last_error_code='fixture-drift'", want: domain.ErrInvalidUploadTaskData},
		{name: "rollback on quota drift", trigger: "fixture_cancel_quota", change: "CREATE TRIGGER fixture_cancel_quota BEFORE UPDATE ON users FOR EACH ROW SET NEW.used_storage=OLD.used_storage+1", want: domain.ErrInvalidAccountData},
		{name: "rollback on object write lock wait", trigger: "fixture_cancel_object", change: "CREATE TRIGGER fixture_cancel_object BEFORE UPDATE ON stored_objects FOR EACH ROW SIGNAL SQLSTATE 'HY000' SET MYSQL_ERRNO=1205, MESSAGE_TEXT='fixture lock wait'", want: domain.ErrUploadCancellationRetryable},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			observation := claim(t, false)
			if fixture.withObject {
				observation.Object = &domain.UploadObjectResult{StorageETag: new("exists"), SizeBytes: 1}
			}
			if fixture.change != "" {
				execute(t, fixture.change)
			}
			if fixture.trigger != "" {
				defer execute(t, "DROP TRIGGER IF EXISTS "+fixture.trigger)
			}
			before := read(t)
			result, err := complete(observation)
			if result != nil || !errors.Is(err, fixture.want) || !reflect.DeepEqual(read(t), before) {
				t.Fatal("拒绝或回滚场景修改了数据，或返回错误类别不符", fixture.want, err)
			}
			if fixture.want == domain.ErrUploadCancellationRetryable {
				var original *mysql.MySQLError
				if !errors.As(err, &original) || original.Number != 1205 {
					t.Fatal("临时收尾错误丢失了数据库原因", err)
				}
			}
		})
	}
}
