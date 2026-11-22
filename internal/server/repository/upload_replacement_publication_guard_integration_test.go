package repository

import (
	"context"
	"crypto/sha256"
	"errors"
	"reflect"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestUploadReplacementPublicationGuardIntegration(t *testing.T) {
	for _, uploadMode := range []string{"SINGLE", "MULTIPART"} {
		for _, binding := range []struct {
			name     string
			verified bool
			locked   bool
		}{
			{name: "both snapshots", verified: true, locked: true},
			{name: "only under lock", locked: true},
			{name: "only verified snapshot", verified: true},
		} {
			t.Run(uploadMode+"/"+binding.name, func(t *testing.T) {
				// 1. 准备已校验字节的任务、仍活动的目标文件和完整预占。
				pool, original := uploadTaskDatabase(t)
				store, err := NewStore(pool)
				if err != nil {
					t.Fatal(err)
				}
				ctx := t.Context()
				execute := func(statement string, arguments ...any) {
					t.Helper()
					if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
						t.Fatal(err)
					}
				}
				digest := domain.UploadContentDigest{SizeBytes: domain.MaxSingleUploadSizeBytes, SHA256: sha256.Sum256([]byte("replacement fixture"))}
				var storageSession, partSize, partCount any
				if uploadMode == "MULTIPART" {
					digest.SizeBytes++
					storageSession, partSize, partCount = "replacement-parts", domain.MaxSingleUploadSizeBytes, 2
				}
				execute("UPDATE users SET used_storage=?,reserved_storage=? WHERE id=7", original.FileSize, digest.SizeBytes)
				execute(`UPDATE stored_objects SET status='VERIFYING',file_size=?,storage_version_id='replacement-version',
					storage_etag='replacement-etag' WHERE id=23`, digest.SizeBytes)
				execute(`UPDATE upload_tasks SET status='VERIFYING',upload_mode=?,storage_upload_id=?,part_size=?,part_count=?,expected_sha256=?,
					file_size=?,reserved_bytes=?,published_object_id=NULL,result_entry_id=NULL,resolved_name=NULL,completed_at=NULL,
					lease_owner='verify-worker',lease_version=3,lease_until=UTC_TIMESTAMP(6)+INTERVAL 1 MINUTE,
					expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 DAY,updated_at=UTC_TIMESTAMP(6) WHERE id=19`,
					uploadMode, storageSession, partSize, partCount, digest.SHA256[:], digest.SizeBytes, digest.SizeBytes)
				setBinding := func(bound bool) {
					t.Helper()
					var entryID, revision any
					if bound {
						entryID, revision = 31, 1
					}
					execute("UPDATE upload_tasks SET replacement_entry_id=?,replacement_expected_revision=? WHERE id=19", entryID, revision)
				}
				readTask := func() *domain.UploadTask {
					t.Helper()
					task, err := store.FindUserUploadTask(ctx, 7, 19)
					if err != nil {
						t.Fatal(err)
					}
					return task
				}
				readObjects := func() [2]*domain.StoredObject {
					t.Helper()
					var objects [2]*domain.StoredObject
					for index, objectID := range [...]uint64{23, 29} {
						object, err := store.FindUserStoredObject(ctx, 7, objectID)
						if err != nil {
							t.Fatal(err)
						}
						objects[index] = object
					}
					return objects
				}
				type publicationState struct {
					entry                    *domain.FileEntry
					used, reserved, sequence uint64
					files, events, history   uint64
				}
				readState := func() publicationState {
					t.Helper()
					entry, err := store.FindUserFileEntry(ctx, 7, 31)
					if err != nil {
						t.Fatal(err)
					}
					state := publicationState{entry: entry}
					if err := pool.QueryRowContext(ctx, `SELECT used_storage,reserved_storage,change_seq,
						(SELECT COUNT(*) FROM file_entries WHERE user_id=7 AND entry_type='FILE'),
						(SELECT COUNT(*) FROM change_events WHERE user_id=7),
						(SELECT COUNT(*) FROM file_versions WHERE user_id=7) FROM users WHERE id=7`).
						Scan(&state.used, &state.reserved, &state.sequence, &state.files, &state.events, &state.history); err != nil {
						t.Fatal(err)
					}
					return state
				}

				// 2. 控制事务外快照与锁内记录是否含替换绑定，使用真实发布入口。
				setBinding(binding.verified)
				verified := readTask()
				setBinding(binding.locked)
				savedTask, objects, state := readTask(), readObjects(), readState()
				callbacks := 0
				publisher := service.NewUploadPublicationService(func(ctx context.Context, task *domain.UploadTask, object *domain.StoredObject, measured domain.UploadContentDigest,
					action func(context.Context, service.UploadPublicationTransaction, *domain.UploadTask, *domain.StoredObject) error) error {
					return store.WithUploadVerificationTransaction(ctx, task, object, measured, func(txContext context.Context, txStore *Store, current *domain.UploadTask, staged *domain.StoredObject) error {
						callbacks++
						if (current.ReplacementEntryID != nil) != binding.locked {
							t.Fatal("锁内任务绑定与夹具不符")
						}
						return action(txContext, txStore, current, staged)
					})
				})

				// 3. 首次和重复发布均被拦截，核对任务、文件、对象、配额及日志。
				for range 2 {
					result, err := publisher.Publish(ctx, verified, objects[0], digest)
					if result != nil || !errors.Is(err, domain.ErrUploadTaskNotReady) {
						t.Fatal("替换任务进入了普通发布或返回了其他错误", result, err)
					}
				}
				if callbacks != 2 || !reflect.DeepEqual(readTask(), savedTask) || !reflect.DeepEqual(readObjects(), objects) ||
					!reflect.DeepEqual(readState(), state) {
					t.Fatal("拦截后未保留原任务、文件、对象、配额、历史或变更事件")
				}
			})
		}
	}
}
