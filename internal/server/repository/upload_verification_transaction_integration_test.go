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

func TestWithUploadVerificationTransactionIntegration(t *testing.T) {
	pool, _ := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	execute := func(t *testing.T, statement string) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	read := func(t *testing.T) (*domain.UploadTask, *domain.StoredObject) {
		t.Helper()
		task, err := store.FindUserUploadTask(ctx, 7, 19)
		if err != nil {
			t.Fatal(err)
		}
		object, err := store.FindUserStoredObject(ctx, 7, 23)
		if err != nil {
			t.Fatal(err)
		}
		return task, object
	}
	reset := func(t *testing.T) (*domain.UploadTask, *domain.StoredObject, domain.UploadContentDigest) {
		t.Helper()
		execute(t, "UPDATE upload_tasks SET status='VERIFYING',upload_mode='SINGLE',storage_upload_id=NULL,part_size=NULL,part_count=NULL,published_object_id=NULL,resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,reserved_bytes=file_size,lease_owner='worker',lease_until=UTC_TIMESTAMP(6)+INTERVAL 1 MINUTE,lease_version=3,expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 DAY,next_attempt_at=NULL,updated_at=UTC_TIMESTAMP(6) WHERE id=19")
		execute(t, "UPDATE stored_objects SET status='VERIFYING',storage_etag='fixture-etag',storage_version_id='null' WHERE id=23")
		task, object := read(t)
		return task, object, domain.UploadContentDigest{SizeBytes: task.FileSize, SHA256: *task.ExpectedSHA256}
	}
	t.Run("rows locked until callback exits", func(t *testing.T) {
		task, object, digest := reset(t)
		err := store.WithUploadVerificationTransaction(ctx, task, object, digest, func(txContext context.Context, _ *Store, current *domain.UploadTask, staged *domain.StoredObject) error {
			if !reflect.DeepEqual(current, task) || !reflect.DeepEqual(staged, object) {
				t.Fatal("当前读回调结果错误")
			}
			// 另一条连接在回调期间立即试锁，三个记录都应被当前事务占有。
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
				err := contender.QueryRowContext(txContext, statement).Scan(&id)
				var lockError *mysql.MySQLError
				if !errors.As(err, &lockError) || lockError.Number != 3572 {
					t.Fatal("另一连接未被预期行锁拒绝", statement, err)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		after, staged := read(t)
		if !reflect.DeepEqual(after, task) || !reflect.DeepEqual(staged, object) {
			t.Fatal("只读复核改变了任务或对象")
		}
	})
	t.Run("callback writes roll back", func(t *testing.T) {
		task, object, digest := reset(t)
		failure := errors.New("fixture write aborted")
		err := store.WithUploadVerificationTransaction(ctx, task, object, digest, func(txContext context.Context, txStore *Store, current *domain.UploadTask, _ *domain.StoredObject) error {
			result := txStore.database.WithContext(txContext).Exec("UPDATE upload_tasks SET attempt_count=attempt_count+1 WHERE user_id=? AND id=?", current.UserID, current.ID)
			if result.Error != nil || result.RowsAffected != 1 {
				t.Fatal("联测未写入预期的单行", result.Error)
			}
			return failure
		})
		after, staged := read(t)
		if !errors.Is(err, failure) || !reflect.DeepEqual(after, task) || !reflect.DeepEqual(staged, object) {
			t.Fatal("复核回调失败后未完整回滚", err)
		}
	})
	t.Run("new lease rejects old verification", func(t *testing.T) {
		task, object, digest := reset(t)
		execute(t, "UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND WHERE id=19")
		candidate := domain.UploadVerificationCandidate{TaskID: task.ID, UserID: task.UserID, StagedObjectID: task.StagedObjectID, LeaseVersion: task.LeaseVersion}
		newLease, err := store.ClaimUploadVerificationLease(ctx, candidate, "new-worker")
		if err != nil || newLease == nil || newLease.LeaseVersion != task.LeaseVersion+1 {
			t.Fatal("真实新租约领取失败", err)
		}
		err = store.WithUploadVerificationTransaction(ctx, task, object, digest, func(context.Context, *Store, *domain.UploadTask, *domain.StoredObject) error {
			t.Fatal("旧租约取得了写回机会")
			return nil
		})
		after, staged := read(t)
		if !errors.Is(err, domain.ErrUploadTaskBusy) || !reflect.DeepEqual(after, newLease) || !reflect.DeepEqual(staged, object) {
			t.Fatal("旧结果未拒绝或改变了新租约", err)
		}
	})
}
