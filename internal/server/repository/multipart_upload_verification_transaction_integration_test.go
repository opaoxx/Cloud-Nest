package repository

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestWithMultipartUploadVerificationTransactionIntegration(t *testing.T) {
	ctx, pool, store := prepareMultipartCompletionDatabase(t)
	const fileSize = domain.MaxSingleUploadSizeBytes + 1
	execute := func(t *testing.T, statement string, arguments ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
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
	// 1. 准备双片合并快照，通过正式领取入口取得校验租约。
	execute(t, "UPDATE users SET reserved_storage=? WHERE id=7", fileSize)
	reset := func(t *testing.T, version *string) (*domain.UploadTask, *domain.StoredObject, domain.UploadContentDigest) {
		t.Helper()
		execute(t, `UPDATE upload_tasks SET status='VERIFYING',upload_mode='MULTIPART',storage_upload_id='Fixture+A/= ',
			file_size=?,part_size=?,part_count=2,expected_sha256=UNHEX(REPEAT('ab',32)),reserved_bytes=?,
			published_object_id=NULL,resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,
			lease_owner=NULL,lease_until=NULL,lease_version=3,next_attempt_at=NULL,
			expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR WHERE id=19`, fileSize, fileSize-1, fileSize)
		execute(t, "UPDATE stored_objects SET status='VERIFYING',file_size=?,storage_etag='fixture-etag',storage_version_id=? WHERE id=23", fileSize, version)
		candidates, err := store.ListMultipartUploadVerificationCandidates(ctx, 1)
		if err != nil || len(candidates) != 1 {
			t.Fatal("未读到分片校验候选", candidates, err)
		}
		task, err := store.ClaimMultipartUploadVerificationLease(ctx, candidates[0], "fixture-worker")
		if err != nil || task == nil || task.ExpectedSHA256 == nil {
			t.Fatal("未取得正式校验租约", err)
		}
		_, object := read(t)
		return task, object, domain.UploadContentDigest{SizeBytes: fileSize, SHA256: *task.ExpectedSHA256}
	}
	assertUnchanged := func(t *testing.T, before *domain.UploadTask, original *domain.StoredObject) {
		t.Helper()
		current, object := read(t)
		if !reflect.DeepEqual(current, before) || !reflect.DeepEqual(object, original) {
			t.Fatal("复核后的任务或对象记录变化")
		}
	}

	// 2. 正确证据进入回调，三行锁保持到回调结束。
	t.Run("locks current records", func(t *testing.T) {
		task, object, digest := reset(t, new("fixture-version"))
		calls := 0
		err := store.WithUploadVerificationTransaction(ctx, task, object, digest, func(txContext context.Context, txStore *Store, current *domain.UploadTask, staged *domain.StoredObject) error {
			calls++
			if txStore.loginUserID != task.UserID || !reflect.DeepEqual(current, task) || !reflect.DeepEqual(staged, object) {
				t.Fatal("回调未取得锁内的当前任务和原对象")
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
		if err != nil || calls != 1 {
			t.Fatal("正确证据未进入一次事务回调", calls, err)
		}
		assertUnchanged(t, task, object)
	})
	t.Run("rollback and optional version", func(t *testing.T) {
		task, object, digest := reset(t, nil)
		rollback := errors.New("fixture requests rollback after a write")
		err := store.WithUploadVerificationTransaction(ctx, task, object, digest, func(txContext context.Context, txStore *Store, current *domain.UploadTask, staged *domain.StoredObject) error {
			if staged.StorageVersionID != nil || staged.StorageETag == nil || *staged.StorageETag != "fixture-etag" {
				t.Fatal("缺省版本未保留合并定位")
			}
			result := txStore.database.WithContext(txContext).Exec("UPDATE upload_tasks SET attempt_count=attempt_count+1 WHERE user_id=? AND id=?", current.UserID, current.ID)
			if result.Error != nil || result.RowsAffected != 1 {
				t.Fatal("未写入待回滚的合成字段", result.Error)
			}
			return rollback
		})
		if !errors.Is(err, rollback) {
			t.Fatal("未保留回调失败原因", err)
		}
		assertUnchanged(t, task, object)
	})

	// 3. 过时、失配和不完整的证据均停在回调之前。
	for _, fixture := range []struct {
		name, change string
		want         error
	}{
		{"wrong measured size", "", domain.ErrUploadBodySizeMismatch},
		{"wrong measured checksum", "", domain.ErrUploadChecksumMismatch},
		{"missing declared checksum", "", domain.ErrInvalidUploadTaskData},
		{"missing snapshot ETag", "", domain.ErrInvalidStoredObjectData},
		{"session loses trailing byte", "UPDATE upload_tasks SET storage_upload_id=RTRIM(storage_upload_id) WHERE id=19", domain.ErrUploadTaskBusy},
		{"session removed", "UPDATE upload_tasks SET storage_upload_id=NULL WHERE id=19", domain.ErrUploadTaskBusy},
		{"lease fenced", "UPDATE upload_tasks SET lease_version=lease_version+1 WHERE id=19", domain.ErrUploadTaskBusy},
		{"lease expired", "UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6) WHERE id=19", domain.ErrUploadTaskBusy},
		{"task expired", "UPDATE upload_tasks SET expires_at=UTC_TIMESTAMP(6) WHERE id=19", domain.ErrUploadTaskNotReady},
		{"plan changed", "UPDATE upload_tasks SET part_count=3 WHERE id=19", domain.ErrInvalidUploadTaskData},
		{"object version changed", "UPDATE stored_objects SET storage_version_id='another-version' WHERE id=23", domain.ErrInvalidStoredObjectData},
		{"object ETag changed", "UPDATE stored_objects SET storage_etag='another-etag' WHERE id=23", domain.ErrInvalidStoredObjectData},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			task, object, digest := reset(t, new("fixture-version"))
			if fixture.change != "" {
				execute(t, fixture.change)
			}
			switch fixture.name {
			case "wrong measured size":
				digest.SizeBytes--
			case "wrong measured checksum":
				digest.SHA256[0] ^= 1
			case "missing declared checksum":
				task.ExpectedSHA256 = nil
			case "missing snapshot ETag":
				object.StorageETag = nil
			}
			before, stored := read(t)
			err := store.WithUploadVerificationTransaction(ctx, task, object, digest, func(context.Context, *Store, *domain.UploadTask, *domain.StoredObject) error {
				t.Fatal("过时或无效证据进入了发布事务回调")
				return nil
			})
			if !errors.Is(err, fixture.want) {
				t.Fatal("错误类型不符", fixture.want, err)
			}
			assertUnchanged(t, before, stored)
		})
	}
	var reserved, used uint64
	if err := pool.QueryRowContext(ctx, "SELECT reserved_storage,used_storage FROM users WHERE id=7").Scan(&reserved, &used); err != nil || reserved != fileSize || used != 0 {
		t.Fatal("复核过程改变了账号配额", reserved, used, err)
	}
	t.Log("有效证据持有账号、任务和暂存对象三行锁，12 个过时或失配场景未进入回调")
}
