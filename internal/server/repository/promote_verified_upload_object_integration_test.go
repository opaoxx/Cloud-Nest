package repository

import (
	"context"
	"crypto/sha256"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestPromoteVerifiedUploadObjectIntegration(t *testing.T) {
	pool, _ := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	contentHash := sha256.Sum256([]byte("verified payload"))
	digest := domain.UploadContentDigest{SizeBytes: domain.MaxSingleUploadSizeBytes, SHA256: contentHash}
	rollbackFailure := errors.New("fixture aborts after promotion")

	for _, scenario := range []struct {
		name       string
		zeroBytes  bool
		duplicate  bool
		change     string
		digestEdit func(*domain.UploadContentDigest)
		wantError  error
	}{
		{name: "ready after reuse miss"},
		{name: "zero bytes and optional metadata", zeroBytes: true},
		{name: "outer transaction rollback", wantError: rollbackFailure},
		{name: "real duplicate key", duplicate: true, wantError: domain.ErrStoredObjectDedupConflict},
		{name: "lease expires inside callback", change: "UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6) WHERE id=19", wantError: domain.ErrUploadTaskBusy},
		{name: "task expires inside callback", change: "UPDATE upload_tasks SET expires_at=UTC_TIMESTAMP(6) WHERE id=19", wantError: domain.ErrUploadTaskBusy},
		{name: "lease version changes", change: "UPDATE upload_tasks SET lease_version=lease_version+1 WHERE id=19", wantError: domain.ErrUploadTaskBusy},
		{name: "lease owner changes by one byte", change: "UPDATE upload_tasks SET lease_owner='verify-worker ' WHERE id=19", wantError: domain.ErrUploadTaskBusy},
		{name: "storage version changes", change: "UPDATE stored_objects SET storage_version_id='another-version' WHERE id=23", wantError: domain.ErrUploadTaskBusy},
		{name: "storage metadata NULL differs from empty", zeroBytes: true, change: "UPDATE stored_objects SET storage_etag='' WHERE id=23", wantError: domain.ErrUploadTaskBusy},
		{name: "wrong size", digestEdit: func(actual *domain.UploadContentDigest) { actual.SizeBytes-- }, wantError: domain.ErrUploadBodySizeMismatch},
		{name: "wrong SHA-256", digestEdit: func(actual *domain.UploadContentDigest) { actual.SHA256[0] ^= 1 }, wantError: domain.ErrUploadChecksumMismatch},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 用正式表建立可发布任务，保留一个独立的已就绪对象作为冲突夹具。
			actual := digest
			var expected, version, etag any = contentHash[:], "fixture-version", "opaque-etag "
			if scenario.duplicate {
				actual.SHA256 = [32]byte{}
				expected = actual.SHA256[:]
			}
			if scenario.zeroBytes {
				actual = domain.UploadContentDigest{SHA256: sha256.Sum256(nil)}
				expected, version, etag = nil, nil, nil
			}
			_, err := pool.ExecContext(ctx, `UPDATE upload_tasks SET status='VERIFYING',upload_mode='SINGLE',
				storage_upload_id=NULL,part_size=NULL,part_count=NULL,published_object_id=NULL,resolved_name=NULL,
				result_entry_id=NULL,completed_at=NULL,file_size=?,reserved_bytes=?,expected_sha256=?,
				lease_owner='verify-worker',lease_until=UTC_TIMESTAMP(6)+INTERVAL 1 MINUTE,lease_version=3,
				expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 DAY,next_attempt_at=NULL,updated_at=UTC_TIMESTAMP(6) WHERE id=19`,
				actual.SizeBytes, actual.SizeBytes, expected)
			if err != nil {
				t.Fatal(err)
			}
			_, err = pool.ExecContext(ctx, `UPDATE stored_objects SET status='VERIFYING',file_size=?,storage_version_id=?,
				storage_etag=?,verified_sha256=NULL,verified_at=NULL,last_error_code='VERIFY_RETRY',updated_at=UTC_TIMESTAMP(6) WHERE id=23`,
				actual.SizeBytes, version, etag)
			if err != nil {
				t.Fatal(err)
			}
			task, err := store.FindUserUploadTask(ctx, 7, 19)
			if err != nil {
				t.Fatal(err)
			}
			staged, err := store.FindUserStoredObject(ctx, 7, 23)
			if err != nil {
				t.Fatal(err)
			}
			if object, err := store.PromoteVerifiedUploadObject(ctx, task, staged, actual); object != nil || err == nil {
				t.Fatal("事务外写入应被拒绝", err)
			}

			// 2. 通过正式复核入口进入事务，再模拟写入前的状态变化。
			var ready *domain.StoredObject
			err = store.WithUploadVerificationTransaction(ctx, task, staged, actual, func(txContext context.Context, txStore *Store, currentTask *domain.UploadTask, currentObject *domain.StoredObject) error {
				if !scenario.duplicate {
					candidate, err := txStore.FindReusableUploadObject(txContext, actual)
					if err != nil || candidate != nil {
						t.Fatal("当前摘要应进入复用未命中分支", err)
					}
				}
				if scenario.change != "" {
					result := txStore.database.WithContext(txContext).Exec(scenario.change)
					if result.Error != nil || result.RowsAffected != 1 {
						t.Fatal("联测未改变预定行", result.Error)
					}
				}
				if scenario.digestEdit != nil {
					scenario.digestEdit(&actual)
				}
				cancelled, stop := context.WithCancel(txContext)
				stop()
				if object, err := txStore.PromoteVerifiedUploadObject(cancelled, currentTask, currentObject, actual); object != nil || !errors.Is(err, context.Canceled) {
					t.Fatal("推进未保留取消原因", err)
				}
				object, err := txStore.PromoteVerifiedUploadObject(txContext, currentTask, currentObject, actual)
				ready = object
				if err == nil && scenario.wantError == rollbackFailure {
					return rollbackFailure
				}
				return err
			})
			if !errors.Is(err, scenario.wantError) {
				t.Fatal("条件推进返回意外结果", err)
			}

			// 3. 在事务结束后核对对象提交或原值回滚，以及任务记录原值。
			saved, err := store.FindUserStoredObject(ctx, 7, 23)
			if err != nil {
				t.Fatal(err)
			}
			if scenario.wantError == nil || scenario.wantError == rollbackFailure {
				if ready == nil || ready.Status != domain.StoredObjectStatusReady ||
					ready.VerifiedSHA256 == nil || *ready.VerifiedSHA256 != actual.SHA256 ||
					ready.DedupHash == nil || *ready.DedupHash != actual.SHA256 ||
					ready.VerifiedAt == nil || !ready.VerifiedAt.Equal(ready.UpdatedAt) ||
					!ready.VerifiedAt.Before(*task.LeaseUntil) || ready.LastErrorCode != nil ||
					!sameUploadStorageText(ready.StorageVersionID, staged.StorageVersionID) ||
					!sameUploadStorageText(ready.StorageETag, staged.StorageETag) {
					t.Fatal("READY 状态或校验事实不完整", ready)
				}
			} else if ready != nil {
				t.Fatal("条件更新失败时返回了对象", ready)
			}
			expectedObject := ready
			if scenario.wantError != nil {
				expectedObject = staged
			}
			if !reflect.DeepEqual(saved, expectedObject) {
				t.Fatal("数据库对象未正确提交或回滚", saved)
			}
			savedTask, err := store.FindUserUploadTask(ctx, 7, 19)
			if err != nil || !reflect.DeepEqual(savedTask, task) {
				t.Fatal("更新影响了上传任务记录", err)
			}
		})
	}
}
