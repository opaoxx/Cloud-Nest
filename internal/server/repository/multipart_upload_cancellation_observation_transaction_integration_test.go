package repository

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
)

func TestWithMultipartUploadCancellationObservationTransactionIntegration(t *testing.T) {
	ctx, pool, store := prepareMultipartCompletionDatabase(t)
	const provider, bucket, key = "local_minio", "fixture-bucket", "fixture/cancel +%.bin"
	const etag, version = "cancel-etag", "Version+A/="
	execute := func(t *testing.T, statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	claim := func(t *testing.T, unbound bool) *domain.MultipartUploadCancellationObservation {
		t.Helper()
		resetMultipartUploadCancellationFixture(t, ctx, pool)
		execute(t, "UPDATE stored_objects SET provider=?,bucket=?,object_key=? WHERE id=23", provider, bucket, key)
		if unbound {
			execute(t, "UPDATE upload_tasks SET storage_upload_id=NULL WHERE id=19")
			execute(t, "UPDATE users SET status='DISABLED' WHERE id=7")
		}
		candidates, err := store.ListMultipartUploadCancellationCandidates(ctx, 1)
		if err != nil || len(candidates) != 1 || candidates[0].TaskID != 19 {
			t.Fatal("未取得观察复核的合成取消候选", candidates, err)
		}
		task, err := store.ClaimMultipartUploadCancellationLease(ctx, candidates[0], "fixture-observation")
		if err != nil {
			t.Fatal("未领取合成租约", err)
		}
		return &domain.MultipartUploadCancellationObservation{Task: *task, ObjectKey: key}
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
		result := facts{task: task, object: object}
		if err := pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage FROM users WHERE id=7").Scan(&result.used, &result.reserved); err != nil {
			t.Fatal(err)
		}
		return result
	}
	objectResult := func(versionID *string, size uint64) *domain.UploadObjectResult {
		return &domain.UploadObjectResult{SizeBytes: size, StorageETag: new(etag), StorageVersionID: versionID}
	}
	const recordVersion = "UPDATE stored_objects SET status='VERIFYING',storage_etag='cancel-etag',storage_version_id='Version+A/=' WHERE id=23"
	const recordNullVersion = "UPDATE stored_objects SET status='VERIFYING',storage_etag='cancel-etag',storage_version_id='null' WHERE id=23"
	const recordWithoutVersion = "UPDATE stored_objects SET status='VERIFYING',storage_etag='cancel-etag',storage_version_id=NULL WHERE id=23"

	// 1. 已写入的版本必须完整匹配。回调执行时三行仍持锁。
	t.Run("versioned observation holds all three locks", func(t *testing.T) {
		observed := claim(t, false)
		execute(t, recordVersion)
		observed.Object = objectResult(new(version), multipartCancellationFixtureSize)
		before := read(t)
		calls := 0
		err := store.WithMultipartUploadCancellationObservationTransaction(ctx, provider, bucket, observed,
			func(txContext context.Context, txStore *Store, task *domain.UploadTask, object *domain.StoredObject) error {
				calls++
				if txStore.loginUserID != 7 || !reflect.DeepEqual(task, before.task) || !reflect.DeepEqual(object, before.object) {
					t.Fatal("回调未收到正确的事务、任务或暂存对象")
				}
				contender, err := pool.BeginTx(txContext, nil)
				if err != nil {
					return err
				}
				defer contender.Rollback()
				for _, statement := range []string{
					"SELECT id FROM users WHERE id=7 FOR UPDATE NOWAIT",
					"SELECT id FROM upload_tasks WHERE id=19 FOR UPDATE NOWAIT",
					"SELECT id FROM stored_objects WHERE id=23 FOR UPDATE NOWAIT",
				} {
					var id uint64
					var lockError *mysql.MySQLError
					if err := contender.QueryRowContext(txContext, statement).Scan(&id); !errors.As(err, &lockError) || lockError.Number != 3572 {
						t.Fatal("第二连接未被观察复核的行锁拦截", statement, err)
					}
				}
				return nil
			})
		if err != nil || calls != 1 || !reflect.DeepEqual(read(t), before) {
			t.Fatal("锁内观察未正确交付或只读回调改变了数据", calls, err)
		}
		t.Log("回调期间，第二连接请求账号、任务和对象三行 NOWAIT 均得到 MySQL 3572")
	})

	// 2. 在拿到对象锁之后推进连接时钟，证明租约在最终判断时必须仍然有效。
	t.Run("lease expires before observation can be accepted", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		now := time.Now().UTC().Truncate(time.Second)
		execute(t, "SET timestamp = ?", now.Unix())
		defer execute(t, "SET timestamp = DEFAULT")
		observed := claim(t, true)
		before := read(t)
		advanceTime := false
		const callback = "fixture:expire_multipart_cancellation_lease_after_object_lock"
		queries := store.database.Callback().Query()
		if err := queries.After("gorm:query").Register(callback, func(query *gorm.DB) {
			if query.Error == nil && query.Statement.Table == "stored_objects" && !advanceTime {
				advanceTime = true
				_, err := query.Statement.ConnPool.ExecContext(query.Statement.Context, "SET timestamp = ?", observed.Task.LeaseUntil.Unix())
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
		err := store.WithMultipartUploadCancellationObservationTransaction(ctx, provider, bucket, observed,
			func(context.Context, *Store, *domain.UploadTask, *domain.StoredObject) error {
				t.Error("到期的租约进入了事务回调")
				return nil
			})
		if !advanceTime || !errors.Is(err, domain.ErrUploadTaskBusy) || !reflect.DeepEqual(read(t), before) {
			t.Fatal("拿到对象锁后未按数据库到期时间拒绝旧观察", err)
		}
		t.Log("拿到对象锁后推进 MySQL 时钟至租约截止，复核返回 UPLOAD_BUSY")
	})

	// 3. 用已绑定和未绑定观察覆盖当前状态、原定位、版本以及记录差异。
	for _, fixture := range []struct {
		name, change string
		unbound      bool
		object       *domain.UploadObjectResult
		edit         func(*domain.MultipartUploadCancellationObservation)
		want         error
	}{
		{name: "bound observation without object"},
		{name: "unbound observation of disabled account", unbound: true},
		{name: "new object retains actual size", unbound: true, object: objectResult(new(version), 29)},
		{name: "already recorded null version", change: recordNullVersion, object: objectResult(new("null"), multipartCancellationFixtureSize)},
		{name: "already recorded unversioned object", change: recordWithoutVersion, object: objectResult(nil, multipartCancellationFixtureSize)},
		{name: "observed missing while a version was recorded", change: recordVersion, want: domain.ErrInvalidStoredObjectData},
		{name: "observed version differs", change: recordVersion, object: objectResult(new("other-version"), multipartCancellationFixtureSize), want: domain.ErrInvalidStoredObjectData},
		{name: "observed nil is not recorded null version", change: recordNullVersion, object: objectResult(nil, multipartCancellationFixtureSize), want: domain.ErrInvalidStoredObjectData},
		{name: "observed size contradicts recorded result", change: recordVersion, object: objectResult(new(version), 29), want: domain.ErrInvalidStoredObjectData},
		{name: "observed ETag differs", change: recordVersion, object: &domain.UploadObjectResult{SizeBytes: multipartCancellationFixtureSize, StorageETag: new("wrong-etag"), StorageVersionID: new(version)}, want: domain.ErrInvalidStoredObjectData},
		{name: "invalid observed metadata", object: &domain.UploadObjectResult{SizeBytes: 29, StorageVersionID: new(version)}, want: domain.ErrInvalidStoredObjectData},
		{name: "lease version replaced", change: "UPDATE upload_tasks SET lease_version=lease_version+1 WHERE id=19", want: domain.ErrUploadTaskBusy},
		{name: "lease owner replaced", change: "UPDATE upload_tasks SET lease_owner='other-worker' WHERE id=19", want: domain.ErrUploadTaskBusy},
		{name: "raw session replaced", change: "UPDATE upload_tasks SET storage_upload_id=RTRIM(storage_upload_id) WHERE id=19", want: domain.ErrUploadTaskBusy},
		{name: "formerly unbound session now bound", unbound: true, change: "UPDATE upload_tasks SET storage_upload_id='later-session' WHERE id=19", want: domain.ErrUploadTaskBusy},
		{name: "retry moved forward", change: "UPDATE upload_tasks SET next_attempt_at=UTC_TIMESTAMP(6)+INTERVAL 1 MINUTE WHERE id=19", want: domain.ErrUploadTaskBusy},
		{name: "different task state", change: "UPDATE upload_tasks SET status='EXPIRING' WHERE id=19", want: domain.ErrUploadTaskNotReady},
		{name: "object reference changed", change: "UPDATE upload_tasks SET staged_object_id=29 WHERE id=19", want: domain.ErrUploadTaskBusy},
		{name: "object key changed", change: "UPDATE stored_objects SET object_key='fixture/replaced' WHERE id=23", want: domain.ErrInvalidStoredObjectData},
		{name: "bucket changed", change: "UPDATE stored_objects SET bucket='different-bucket' WHERE id=23", want: domain.ErrInvalidStoredObjectData},
		{name: "cross account", edit: func(observed *domain.MultipartUploadCancellationObservation) { observed.Task.UserID = 8 }, want: domain.ErrUploadTaskNotFound},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			observed := claim(t, fixture.unbound)
			observed.Object = fixture.object
			if fixture.change != "" {
				execute(t, fixture.change)
			}
			if fixture.edit != nil {
				fixture.edit(observed)
			}
			before := read(t)
			called := false
			err := store.WithMultipartUploadCancellationObservationTransaction(ctx, provider, bucket, observed,
				func(_ context.Context, _ *Store, task *domain.UploadTask, object *domain.StoredObject) error {
					called = true
					if !reflect.DeepEqual(task, before.task) || !reflect.DeepEqual(object, before.object) {
						t.Fatal("事务回调未收到锁内记录")
					}
					return nil
				})
			if !errors.Is(err, fixture.want) || called != (fixture.want == nil) || !reflect.DeepEqual(read(t), before) {
				t.Fatal("观察事务结果或只读边界不符", fixture.want, err, called)
			}
		})
	}
}
