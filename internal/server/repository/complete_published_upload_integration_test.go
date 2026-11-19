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

func TestCompletePublishedUploadIntegration(t *testing.T) {
	pool, originalTask := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	execute := func(t *testing.T, statement string, arguments ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
			t.Fatal(err)
		}
	}
	occupiedKey, err := service.BuildEntryNameKey(originalTask.RequestedName)
	if err != nil {
		t.Fatal(err)
	}
	execute(t, "INSERT INTO file_entries(user_id,parent_id,name,name_key,entry_type) VALUES(7,13,?,?,'DIR')", originalTask.RequestedName, occupiedKey)
	readQuota := func(t *testing.T) (uint64, uint64) {
		t.Helper()
		var used, reserved uint64
		if err := pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage FROM users WHERE id=7").Scan(&used, &reserved); err != nil {
			t.Fatal(err)
		}
		return used, reserved
	}
	rollbackFixture := errors.New("fixture interrupts completed publication")

	for _, scenario := range []struct {
		name           string
		chooseName     bool
		skipSettlement bool
		wrongObject    bool
		changeSQL      string
		changeFile     string
		zero           bool
		wantError      error
	}{
		{name: "reuse object and save numbered name", chooseName: true},
		{name: "unsettled reservation", skipSettlement: true, wantError: domain.ErrInvalidUploadTaskData},
		{name: "file references different object", wrongObject: true, wantError: domain.ErrUploadTaskBusy},
		{name: "result name byte mismatch", changeFile: "UPDATE file_entries SET name=CONCAT(name,' ') WHERE id=?", wantError: domain.ErrUploadTaskBusy},
		{name: "result source device mismatch", changeFile: "UPDATE file_entries SET source_device_id=NULL WHERE id=?", wantError: domain.ErrUploadTaskBusy},
		{name: "published object no longer ready", changeSQL: "UPDATE stored_objects SET status='DELETING' WHERE id=29", wantError: domain.ErrUploadTaskBusy},
		{name: "verification lease expired", changeSQL: "UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6) WHERE id=19", wantError: domain.ErrUploadTaskBusy},
		{name: "task expired", changeSQL: "UPDATE upload_tasks SET expires_at=UTC_TIMESTAMP(6) WHERE id=19", wantError: domain.ErrUploadTaskBusy},
		{name: "new verification lease version", changeSQL: "UPDATE upload_tasks SET lease_version=lease_version+1 WHERE id=19", wantError: domain.ErrUploadTaskBusy},
		{name: "rollback after completion", wantError: rollbackFixture},
		{name: "complete zero-byte file with new object", zero: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 准备真实可校验任务、预占和旧错误，保留已有 READY 对象 29。
			digest := domain.UploadContentDigest{SizeBytes: domain.MaxSingleUploadSizeBytes}
			if scenario.wantError == rollbackFixture {
				digest.SHA256[0] = 0xff
			}
			if scenario.zero {
				digest = domain.UploadContentDigest{SHA256: sha256.Sum256(nil)}
			}
			execute(t, "UPDATE users SET used_storage=?,reserved_storage=? WHERE id=7", domain.MaxSingleUploadSizeBytes, digest.SizeBytes)
			execute(t, "UPDATE stored_objects SET status='VERIFYING',file_size=?,verified_sha256=NULL,verified_at=NULL,unreferenced_at=NULL,gc_after=NULL WHERE id=23", digest.SizeBytes)
			execute(t, `UPDATE upload_tasks SET status='VERIFYING',upload_mode='SINGLE',storage_upload_id=NULL,part_size=NULL,part_count=NULL,
				published_object_id=NULL,resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,
				file_size=?,reserved_bytes=?,expected_sha256=?,lease_owner='verify-worker',lease_version=3,
				lease_until=UTC_TIMESTAMP(6)+INTERVAL 1 MINUTE,expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 DAY,updated_at=UTC_TIMESTAMP(6),
				next_attempt_at=UTC_TIMESTAMP(6)-INTERVAL 1 MINUTE,last_error_code='VERIFY_RETRY',last_error_message='retry fixture'
				WHERE id=19`, digest.SizeBytes, digest.SizeBytes, digest.SHA256[:])
			task, err := store.FindUserUploadTask(ctx, 7, 19)
			if err != nil {
				t.Fatal(err)
			}
			staged, err := store.FindUserStoredObject(ctx, 7, task.StagedObjectID)
			if err != nil {
				t.Fatal(err)
			}

			// 2. 使用现有发布步骤创建文件并结算，再写回当前任务的完成结果。
			var result *domain.UploadTask
			var input *domain.CreatePublishedFileEntryInput
			err = store.WithUploadVerificationTransaction(ctx, task, staged, digest, func(txContext context.Context, txStore *Store, current *domain.UploadTask, currentObject *domain.StoredObject) error {
				selected, err := txStore.SelectUploadPublicationObject(txContext, current, currentObject, digest)
				if err != nil {
					return err
				}
				source, err := txStore.FindUploadSourceDevice(txContext, current)
				if err != nil {
					return err
				}
				name := scenario.name + ".PDF"
				if scenario.chooseName {
					name, err = service.ResolveUploadPublicationName(txContext, current.UserID, current.TargetParentID, current.RequestedName, txStore.FindUserActiveFileEntryByName)
					if err != nil {
						return err
					}
					expectedName, err := service.BuildUploadConflictName(current.RequestedName, 1)
					if err != nil || name != expectedName {
						t.Fatal("联测未选到预期编号名称", name, err)
					}
				}
				input, err = service.BuildPublishedFileEntryInput(current, selected, name, source)
				if err != nil {
					return err
				}
				entry, err := txStore.CreatePublishedFileEntry(txContext, current, *input)
				if err != nil {
					return err
				}
				if !scenario.skipSettlement {
					if err := txStore.SettlePublishedUploadQuota(txContext, current); err != nil {
						return err
					}
				}
				if scenario.changeSQL != "" {
					if err := txStore.database.WithContext(txContext).Exec(scenario.changeSQL).Error; err != nil {
						return err
					}
				}
				if scenario.changeFile != "" {
					if err := txStore.database.WithContext(txContext).Exec(scenario.changeFile, entry.ID).Error; err != nil {
						return err
					}
				}
				objectID := selected.ID
				if scenario.wrongObject {
					objectID = current.StagedObjectID
				}
				result, err = txStore.CompletePublishedUpload(txContext, current, objectID, entry)
				if err != nil {
					return err
				}
				if scenario.wantError == rollbackFixture {
					return rollbackFixture
				}
				orphaned, err := txStore.MarkDeduplicatedUploadObjectOrphaned(txContext, result.ID)
				if err != nil {
					return err
				}
				if selected.ID != current.StagedObjectID {
					if orphaned == nil || orphaned.ID != current.StagedObjectID || orphaned.Status != domain.StoredObjectStatusOrphaned {
						t.Fatal("复用对象后的副本未标记为孤立", orphaned)
					}
				} else if orphaned != nil {
					t.Fatal("本次发布对象不应成为孤立副本", orphaned)
				}
				if retried, err := txStore.CompletePublishedUpload(txContext, current, objectID, entry); retried != nil || !errors.Is(err, domain.ErrUploadTaskNotReady) {
					t.Fatal("完成任务再次被改写", retried, err)
				}
				return nil
			})
			if !errors.Is(err, scenario.wantError) || input == nil {
				t.Fatal("上传发布结果错误", err)
			}

			// 3. 在事务外核对完成事实，错误分支须撤销文件、对象和配额变更。
			after, err := store.FindUserUploadTask(ctx, task.UserID, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			entry, entryError := store.FindUserActiveFileEntryByName(ctx, task.UserID, task.TargetParentID, input.NameKey)
			used, reserved := readQuota(t)
			if scenario.wantError != nil {
				if (scenario.wantError != rollbackFixture && result != nil) || !reflect.DeepEqual(after, task) || entry != nil ||
					!errors.Is(entryError, domain.ErrFileEntryNotFound) || used != domain.MaxSingleUploadSizeBytes || reserved != digest.SizeBytes {
					t.Fatal("失败后的文件、任务或配额未完整回滚", entry, entryError, after)
				}
				afterObject, err := store.FindUserStoredObject(ctx, task.UserID, task.StagedObjectID)
				if err != nil || !reflect.DeepEqual(afterObject, staged) {
					t.Fatal("失败后暂存对象未还原", err)
				}
				return
			}
			if entryError != nil || entry == nil || result == nil || !reflect.DeepEqual(after, result) ||
				after.Status != domain.UploadStatusCompleted || after.ResolvedName == nil || *after.ResolvedName != input.Name ||
				after.PublishedObjectID == nil || *after.PublishedObjectID != input.ObjectID ||
				after.ResultEntryID == nil || *after.ResultEntryID != entry.ID ||
				after.CompletedAt == nil || !after.CompletedAt.Equal(after.UpdatedAt) || after.CompletedAt.Before(entry.CreatedAt) ||
				after.ReservedBytes != 0 || after.LeaseOwner != nil || after.LeaseUntil != nil ||
				after.LeaseVersion != task.LeaseVersion || after.NextAttemptAt != nil || after.LastErrorCode != nil || after.LastErrorMessage != nil ||
				used != domain.MaxSingleUploadSizeBytes+digest.SizeBytes || reserved != 0 {
				t.Fatal("完成任务、文件结果或配额未正确提交", after, entry, entryError)
			}
		})
	}
}
