package repository

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestCompleteExistingMultipartUploadCancellationIntegration(t *testing.T) {
	ctx, pool, store := prepareMultipartCompletionDatabase(t)
	const provider, bucket, key, etag, version = "local_minio", "fixture-bucket", "fixture/cancel exists +%.bin", "Opaque-ETag", "Version+A/="
	execute := func(t *testing.T, statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	claim := func(t *testing.T, observed *domain.UploadObjectResult) *domain.MultipartUploadCancellationObservation {
		t.Helper()
		resetMultipartUploadCancellationFixture(t, ctx, pool)
		execute(t, "UPDATE file_entries SET object_id=29 WHERE id=31")
		execute(t, "UPDATE stored_objects SET provider=?,bucket=?,object_key=? WHERE id=23", provider, bucket, key)
		candidates, err := store.ListMultipartUploadCancellationCandidates(ctx, 1)
		if err != nil || len(candidates) != 1 || candidates[0].TaskID != 19 {
			t.Fatal("未取得合成取消候选", candidates, err)
		}
		task, err := store.ClaimMultipartUploadCancellationLease(ctx, candidates[0], "fixture-existing-cancel")
		if err != nil {
			t.Fatal("未取得合成取消租约", err)
		}
		return &domain.MultipartUploadCancellationObservation{Task: *task, ObjectKey: key, Object: observed}
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
		return store.CompleteExistingMultipartUploadCancellation(ctx, provider, bucket, observation)
	}
	checkCommitted := func(t *testing.T, before facts, observation *domain.MultipartUploadCancellationObservation, finished *domain.UploadTask) facts {
		t.Helper()
		after := read(t)
		if finished == nil || finished.Status != domain.UploadStatusCancelled || !reflect.DeepEqual(after.task, finished) ||
			finished.CompletedAt == nil || finished.ReservedBytes != 0 || after.used != before.used ||
			after.reserved != before.reserved-before.task.ReservedBytes || !after.accountUpdated.Equal(*finished.CompletedAt) {
			t.Fatal("终态、账号已用或任务预占结算不符", finished, after.used, after.reserved)
		}
		orphaned := after.object
		if orphaned.UnreferencedAt == nil || orphaned.GCAfter == nil || orphaned.UnreferencedAt.Before(*finished.CompletedAt) ||
			!orphaned.GCAfter.Equal(orphaned.UnreferencedAt.Add(7*24*time.Hour)) {
			t.Fatal("对象回收期限不符", orphaned)
		}
		expected := *before.object
		expected.Status, expected.UnreferencedAt, expected.GCAfter = domain.StoredObjectStatusOrphaned, orphaned.UnreferencedAt, orphaned.GCAfter
		expected.UpdatedAt = *orphaned.UnreferencedAt
		expected.StorageETag, expected.StorageVersionID = observation.Object.StorageETag, observation.Object.StorageVersionID
		if !reflect.DeepEqual(orphaned, &expected) {
			t.Fatal("待回收对象没有精确保留位置、原声明及新观察的版本")
		}
		return after
	}
	present := func(id *string, size uint64) *domain.UploadObjectResult {
		return &domain.UploadObjectResult{SizeBytes: size, StorageETag: new(etag), StorageVersionID: id}
	}

	// 1. 暂存记录尚无元数据时保存观察，并验证回收入口能领到精确版本。
	t.Run("register exact version and reread after recovery claim", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		now := time.Now().UTC().Truncate(time.Second)
		execute(t, "SET timestamp = ?", now.Unix())
		defer execute(t, "SET timestamp = DEFAULT")
		observation := claim(t, present(new(version), 29))
		before := read(t)
		finished, err := complete(observation)
		if err != nil {
			t.Fatal(err)
		}
		after := checkCommitted(t, before, observation, finished)
		if candidates, err := store.ListStoredObjectGCCandidates(ctx, 1); err != nil || len(candidates) != 0 {
			t.Fatal("回收宽限开始时不应领取对象", candidates, err)
		}
		execute(t, "SET timestamp = ?", after.object.GCAfter.Unix())
		candidates, err := store.ListStoredObjectGCCandidates(ctx, 1)
		if err != nil || len(candidates) != 1 || candidates[0].ObjectID != 23 {
			t.Fatal("到期回收候选缺失", candidates, err)
		}
		claimed, err := store.ClaimStoredObjectGC(ctx, candidates[0])
		if err != nil || claimed == nil || claimed.ObjectKey != key || claimed.StorageVersionID == nil || *claimed.StorageVersionID != version ||
			claimed.StorageETag == nil || *claimed.StorageETag != etag {
			t.Fatal("回收入口未取得确切版本", err)
		}
		replayBefore := read(t)
		replayed, err := complete(observation)
		if err != nil || !reflect.DeepEqual(replayed, finished) || !reflect.DeepEqual(replayBefore, read(t)) {
			t.Fatal("回收待执行时的重试改变了原提交记录", err)
		}
		wrongVersion := *observation
		wrongVersion.Object = present(new("other-version"), 29)
		if result, err := complete(&wrongVersion); result != nil || !errors.Is(err, domain.ErrInvalidStoredObjectData) {
			t.Fatal("另一版本错误地复用了原终态", err)
		}
		missing := *observation
		missing.Object = nil
		if result, err := store.CompleteMissingMultipartUploadCancellation(ctx, provider, bucket, &missing); result != nil ||
			!errors.Is(err, domain.ErrInvalidStoredObjectData) || !reflect.DeepEqual(replayBefore, read(t)) {
			t.Fatal("缺失观察错误地复用了已登记对象的终态", err)
		}
		t.Logf("七天后回收领取保留版本与 ETag，终态重试后已用 %d B、预占 %d B", replayBefore.used, replayBefore.reserved)
	})

	// 2. 复用精确的已知元数据，并覆盖可空版本和条件更新失败后的回滚。
	for _, fixture := range []struct {
		name, change, trigger string
		observed              *domain.UploadObjectResult
		want                  error
	}{
		{name: "recorded verifying version", observed: present(new(version), multipartCancellationFixtureSize),
			change: "UPDATE stored_objects SET status='VERIFYING',storage_etag='Opaque-ETag',storage_version_id='Version+A/=' WHERE id=23"},
		{name: "no version ID", observed: present(nil, 29)},
		{name: "explicit null version", observed: present(new("null"), 29)},
		{name: "reject missing observation object", want: domain.ErrUploadTaskNotReady},
		{name: "reject stale version", observed: present(new("stale-version"), multipartCancellationFixtureSize),
			change: "UPDATE stored_objects SET status='VERIFYING',storage_etag='Opaque-ETag',storage_version_id='Version+A/=' WHERE id=23", want: domain.ErrInvalidStoredObjectData},
		{name: "retain referenced object", observed: present(new(version), 29),
			change: "UPDATE file_entries SET object_id=23 WHERE id=31", want: domain.ErrStoredObjectInUse},
		{name: "rollback changed stored version", observed: present(new(version), 29), trigger: "fixture_existing_cancel_version",
			change: "CREATE TRIGGER fixture_existing_cancel_version BEFORE UPDATE ON stored_objects FOR EACH ROW SET NEW.storage_version_id='wrong-version'", want: domain.ErrInvalidStoredObjectData},
		{name: "rollback version write and quota on recovery registration failure", observed: present(new(version), 29), trigger: "fixture_existing_cancel_orphan",
			change: `CREATE TRIGGER fixture_existing_cancel_orphan BEFORE UPDATE ON stored_objects FOR EACH ROW
				BEGIN IF NEW.status='ORPHANED' THEN SIGNAL SQLSTATE 'HY000' SET MYSQL_ERRNO=1205, MESSAGE_TEXT='fixture lock wait'; END IF; END`,
			want: domain.ErrUploadCancellationRetryable},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			observation := claim(t, fixture.observed)
			if fixture.change != "" {
				execute(t, fixture.change)
			}
			if fixture.trigger != "" {
				defer execute(t, "DROP TRIGGER IF EXISTS "+fixture.trigger)
			}
			before := read(t)
			result, err := complete(observation)
			if !errors.Is(err, fixture.want) {
				t.Fatal("取消终态错误类别不符", fixture.want, err)
			}
			if fixture.want != nil {
				if result != nil || !reflect.DeepEqual(before, read(t)) {
					t.Fatal("拒绝后留下了任务、账目或对象修改")
				}
				if fixture.want == domain.ErrUploadCancellationRetryable {
					var original *mysql.MySQLError
					if !errors.As(err, &original) || original.Number != 1205 {
						t.Fatal("回收登记失败丢失了原数据库错误", err)
					}
				}
				return
			}
			after := checkCommitted(t, before, observation, result)
			replayed, err := complete(observation)
			if err != nil || !reflect.DeepEqual(replayed, result) || !reflect.DeepEqual(after, read(t)) {
				t.Fatal("已提交结果未稳定复用", err)
			}
		})
	}
}
