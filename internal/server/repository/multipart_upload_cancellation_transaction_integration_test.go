package repository

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestWithMultipartUploadCancellationTransactionIntegration(t *testing.T) {
	ctx, pool, store := prepareMultipartCompletionDatabase(t)
	const fileSize = domain.MaxSingleUploadSizeBytes + 1
	const rawSession = "Fixture+A/= "
	execute := func(t *testing.T, statement string, arguments ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
			t.Fatal(err)
		}
	}
	reset := func(t *testing.T) {
		t.Helper()
		// 1. 构造已受理的分片任务，原任务期限已过，但仍保留会话、对象和预占。
		execute(t, `UPDATE stored_objects SET status='STAGING',file_size=?,storage_etag=NULL,storage_version_id=NULL,
			verified_at=NULL,verified_sha256=NULL,unreferenced_at=NULL,gc_after=NULL WHERE id=23`, fileSize)
		execute(t, `UPDATE users SET status='ACTIVE',used_storage=1024,reserved_storage=? WHERE id=7`, fileSize+512)
		execute(t, `UPDATE upload_tasks SET status='CANCELLING',upload_mode='MULTIPART',staged_object_id=23,storage_upload_id=?,
			file_size=?,part_size=?,part_count=2,expected_sha256=UNHEX(REPEAT('ab',32)),reserved_bytes=?,
			published_object_id=NULL,resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,
			lease_owner=NULL,lease_until=NULL,lease_version=4,next_attempt_at=NULL,
			expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND,updated_at=UTC_TIMESTAMP(6) WHERE id=19`, rawSession, fileSize, fileSize-1, fileSize)
	}
	scan := func(t *testing.T) domain.UploadCancellationCandidate {
		t.Helper()
		candidates, err := store.ListMultipartUploadCancellationCandidates(ctx, 1)
		want := domain.UploadCancellationCandidate{TaskID: 19, UserID: 7, StagedObjectID: 23, LeaseVersion: 4}
		if err != nil || len(candidates) != 1 || candidates[0] != want {
			t.Fatal("未取得待复核的分片取消候选", candidates, err)
		}
		return candidates[0]
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

	// 2. 真实第二连接复核三行锁，事务结束后检查快照与配额。
	t.Run("lock account task and verifying object", func(t *testing.T) {
		reset(t)
		execute(t, "UPDATE stored_objects SET status='VERIFYING',storage_etag='fixture-etag',storage_version_id='fixture-version' WHERE id=23")
		candidate, before := scan(t), read(t)
		calls := 0
		err := store.WithMultipartUploadCancellationTransaction(ctx, candidate, func(txContext context.Context, txStore *Store, task *domain.UploadTask, object *domain.StoredObject) error {
			calls++
			if txStore.loginUserID != candidate.UserID || !reflect.DeepEqual(task, before.task) || !reflect.DeepEqual(object, before.object) {
				t.Fatal("回调未收到正确的事务、任务或对象")
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
					t.Fatal("另一连接未被事务行锁拦住", statement, err)
				}
			}
			return nil
		})
		if err != nil || calls != 1 || !reflect.DeepEqual(read(t), before) {
			t.Fatal("只读复核未正常回调或修改了数据", calls, err)
		}
	})
	t.Run("due at database time without a bound session", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		now := time.Now().UTC().Truncate(time.Second)
		execute(t, "SET timestamp = ?", now.Unix())
		defer execute(t, "SET timestamp = DEFAULT")
		reset(t)
		execute(t, `UPDATE upload_tasks SET storage_upload_id=NULL,lease_owner='old-cleanup',
			lease_until=UTC_TIMESTAMP(6),next_attempt_at=UTC_TIMESTAMP(6) WHERE id=19`)
		execute(t, "UPDATE users SET status='DISABLED' WHERE id=7")
		candidate, before := scan(t), read(t)
		callbackError := errors.New("fixture read-only callback stopped")
		err := store.WithMultipartUploadCancellationTransaction(ctx, candidate, func(_ context.Context, _ *Store, task *domain.UploadTask, object *domain.StoredObject) error {
			if task.StorageUploadID != nil || object.Status != domain.StoredObjectStatusStaging || !reflect.DeepEqual(task, before.task) {
				t.Fatal("无会话、到点的取消未按原记录交给回调")
			}
			return callbackError
		})
		if !errors.Is(err, callbackError) || !reflect.DeepEqual(read(t), before) {
			t.Fatal("未保留已受理取消及回调的原始结果", err)
		}
	})

	// 3. 扫描后状态或定位变化时拒绝进入回调，数据库保持调用前的事实。
	for _, scenario := range []struct {
		name, change string
		want         error
	}{
		{"status changed", "UPDATE upload_tasks SET status='EXPIRING' WHERE id=19", domain.ErrUploadTaskNotReady},
		{"mode changed", "UPDATE upload_tasks SET upload_mode='SINGLE',storage_upload_id=NULL,part_size=NULL,part_count=NULL WHERE id=19", domain.ErrUploadTaskNotReady},
		{"object reference changed", "UPDATE upload_tasks SET staged_object_id=29 WHERE id=19", domain.ErrUploadTaskBusy},
		{"lease replaced", "UPDATE upload_tasks SET lease_version=lease_version+1 WHERE id=19", domain.ErrUploadTaskBusy},
		{"active lease", "UPDATE upload_tasks SET lease_owner='current-cleanup',lease_until=UTC_TIMESTAMP(6)+INTERVAL 1 MINUTE WHERE id=19", domain.ErrUploadTaskBusy},
		{"retry postponed", "UPDATE upload_tasks SET next_attempt_at=UTC_TIMESTAMP(6)+INTERVAL 1 MINUTE WHERE id=19", domain.ErrUploadTaskBusy},
		{"incomplete reservation", "UPDATE upload_tasks SET reserved_bytes=reserved_bytes-1 WHERE id=19", domain.ErrInvalidUploadTaskData},
		{"object size changed", "UPDATE stored_objects SET file_size=file_size+1 WHERE id=23", domain.ErrInvalidStoredObjectData},
		{"object became ready", "UPDATE stored_objects SET status='READY',verified_sha256=UNHEX(REPEAT('ac',32)),verified_at=UTC_TIMESTAMP(6) WHERE id=23", domain.ErrInvalidStoredObjectData},
		{"other account", "", domain.ErrUploadTaskNotFound},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			reset(t)
			candidate := scan(t)
			if scenario.change == "" {
				candidate.UserID = 8
			} else {
				execute(t, scenario.change)
			}
			before := read(t)
			err := store.WithMultipartUploadCancellationTransaction(ctx, candidate, func(context.Context, *Store, *domain.UploadTask, *domain.StoredObject) error {
				t.Error("过时或无效候选进入了事务回调")
				return nil
			})
			if !errors.Is(err, scenario.want) || !reflect.DeepEqual(read(t), before) {
				t.Fatal("拒绝结果或只读事务边界不符", scenario.want, err)
			}
		})
	}
}
