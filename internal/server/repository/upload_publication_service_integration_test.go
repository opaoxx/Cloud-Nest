package repository

import (
	"context"
	"crypto/sha256"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestUploadPublicationServiceIntegration(t *testing.T) {
	outerFailure := errors.New("fixture abort after publication callback")
	for _, scenario := range []struct {
		name          string
		reuse         bool
		zeroBytes     bool
		blockedTarget bool
		shortReserve  bool
		protectedCopy bool
		wantError     error
	}{
		{name: "publish newly verified object"},
		{name: "reuse object and numbered name", reuse: true},
		{name: "publish zero bytes", zeroBytes: true},
		{name: "ancestor in trash", blockedTarget: true, wantError: domain.ErrUploadTargetUnavailable},
		{name: "quota mismatch after file creation", shortReserve: true, wantError: domain.ErrInvalidAccountData},
		{name: "orphaning fails after task completion", reuse: true, protectedCopy: true, wantError: domain.ErrStoredObjectInUse},
		{name: "outer rollback after cleanup", reuse: true, wantError: outerFailure},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 建立可校验任务、各自独立的 MySQL 记录及必要的失败条件。
			pool, original := uploadTaskDatabase(t)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}
			execute := func(statement string, args ...any) {
				t.Helper()
				if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
					t.Fatal(err)
				}
			}
			execute(`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type)
				VALUES(15,7,13,'ancestor',CAST('ancestor' AS BINARY),'DIR'),(16,7,15,'target',CAST('target' AS BINARY),'DIR')`)
			execute("UPDATE file_entries SET status='PURGED',object_id=NULL,purged_at=UTC_TIMESTAMP(6) WHERE id=31")
			execute("UPDATE stored_objects SET unreferenced_at=UTC_TIMESTAMP(6)-INTERVAL 1 DAY,gc_after=UTC_TIMESTAMP(6)+INTERVAL 6 DAY WHERE id=29")
			digest := domain.UploadContentDigest{SizeBytes: domain.MaxSingleUploadSizeBytes, SHA256: sha256.Sum256([]byte("new upload fixture"))}
			if scenario.reuse {
				digest.SHA256 = [32]byte{}
				nameKey, err := service.BuildEntryNameKey(original.RequestedName)
				if err != nil {
					t.Fatal(err)
				}
				execute("INSERT INTO file_entries(user_id,parent_id,name,name_key,entry_type) VALUES(7,16,?,?,'DIR')", original.RequestedName, nameKey)
			}
			if scenario.zeroBytes {
				digest = domain.UploadContentDigest{SHA256: sha256.Sum256(nil)}
			}
			reserved := digest.SizeBytes
			if scenario.shortReserve {
				reserved--
			}
			execute("UPDATE users SET reserved_storage=? WHERE id=7", reserved)
			execute("UPDATE stored_objects SET status='VERIFYING',file_size=? WHERE id=23", digest.SizeBytes)
			execute(`UPDATE upload_tasks SET target_parent_id=16,status='VERIFYING',upload_mode='SINGLE',
				storage_upload_id=NULL,part_size=NULL,part_count=NULL,published_object_id=NULL,resolved_name=NULL,result_entry_id=NULL,
				completed_at=NULL,file_size=?,reserved_bytes=?,expected_sha256=?,lease_owner='verify-worker',lease_version=3,
				lease_until=UTC_TIMESTAMP(6)+INTERVAL 1 MINUTE,expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 DAY,updated_at=UTC_TIMESTAMP(6) WHERE id=19`,
				digest.SizeBytes, digest.SizeBytes, digest.SHA256[:])
			if scenario.blockedTarget {
				execute("UPDATE file_entries SET status='TRASHED',deleted_at=UTC_TIMESTAMP(6) WHERE id=15")
			}
			if scenario.protectedCopy {
				execute(`INSERT INTO file_entries(user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,status,deleted_at)
					VALUES(7,13,'protected fixture',CAST('protected fixture' AS BINARY),'FILE','OTHER',23,?,'TRASHED',UTC_TIMESTAMP(6))`, digest.SizeBytes)
			}
			task, err := store.FindUserUploadTask(ctx, 7, 19)
			if err != nil {
				t.Fatal(err)
			}
			readObject := func(objectID uint64) *domain.StoredObject {
				t.Helper()
				object, err := store.FindUserStoredObject(ctx, 7, objectID)
				if err != nil {
					t.Fatal(err)
				}
				return object
			}
			staged, reusable := readObject(23), readObject(29)
			expectedName, objectID := task.RequestedName, staged.ID
			if scenario.reuse {
				expectedName, err = service.BuildUploadConflictName(task.RequestedName, 1)
				if err != nil {
					t.Fatal(err)
				}
				objectID = reusable.ID
			}
			nameKey, err := service.BuildEntryNameKey(expectedName)
			if err != nil {
				t.Fatal(err)
			}

			// 2. 把真实写回复核入口接给发布服务，直接调用正式编排方法。
			publisher := service.NewUploadPublicationService(func(ctx context.Context, task *domain.UploadTask, object *domain.StoredObject, measured domain.UploadContentDigest,
				action func(context.Context, service.UploadPublicationTransaction, *domain.UploadTask, *domain.StoredObject) error) error {
				return store.WithUploadVerificationTransaction(ctx, task, object, measured, func(txContext context.Context, txStore *Store, current *domain.UploadTask, staged *domain.StoredObject) error {
					if err := action(txContext, txStore, current, staged); err != nil {
						return err
					}
					if scenario.wantError == outerFailure {
						return outerFailure
					}
					return nil
				})
			})
			result, err := publisher.Publish(ctx, task, staged, digest)
			if !errors.Is(err, scenario.wantError) {
				t.Fatal("发布服务结果错误", err)
			}

			// 3. 从事务外确认文件、用量、任务及两个对象的提交或回滚。
			savedTask, err := store.FindUserUploadTask(ctx, 7, 19)
			if err != nil {
				t.Fatal(err)
			}
			entry, entryError := store.FindUserActiveFileEntryByName(ctx, 7, 16, nameKey)
			var savedUsed, savedReserved uint64
			if err := pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage FROM users WHERE id=7").Scan(&savedUsed, &savedReserved); err != nil {
				t.Fatal(err)
			}
			if scenario.wantError != nil {
				if result != nil || !reflect.DeepEqual(savedTask, task) || entry != nil || !errors.Is(entryError, domain.ErrFileEntryNotFound) ||
					savedUsed != 0 || savedReserved != reserved || !reflect.DeepEqual(readObject(23), staged) || !reflect.DeepEqual(readObject(29), reusable) {
					t.Fatal("失败发布留下了持久化变更", result, savedTask, entry, entryError)
				}
				return
			}
			if result == nil || !reflect.DeepEqual(result, savedTask) || result.Status != domain.UploadStatusCompleted ||
				result.PublishedObjectID == nil || *result.PublishedObjectID != objectID ||
				result.ResolvedName == nil || *result.ResolvedName != expectedName ||
				entryError != nil || entry == nil || result.ResultEntryID == nil || *result.ResultEntryID != entry.ID ||
				savedUsed != digest.SizeBytes || savedReserved != 0 {
				t.Fatal("发布完成任务、文件或配额不符", result, entry, entryError)
			}
			published := readObject(objectID)
			if published.Status != domain.StoredObjectStatusReady || published.VerifiedSHA256 == nil || *published.VerifiedSHA256 != digest.SHA256 ||
				published.UnreferencedAt != nil || published.GCAfter != nil {
				t.Fatal("发布对象及回收计时不符", published)
			}
			if scenario.reuse {
				orphaned := readObject(staged.ID)
				if orphaned.Status != domain.StoredObjectStatusOrphaned || orphaned.UnreferencedAt == nil ||
					orphaned.GCAfter == nil || !orphaned.GCAfter.Equal(orphaned.UnreferencedAt.Add(7*24*time.Hour)) {
					t.Fatal("去重暂存副本未开始七天计时", orphaned)
				}
			}
			if retried, err := publisher.Publish(ctx, task, staged, digest); retried != nil || !errors.Is(err, domain.ErrUploadTaskNotReady) {
				t.Fatal("旧校验快照重复通过发布", retried, err)
			}
		})
	}
}
