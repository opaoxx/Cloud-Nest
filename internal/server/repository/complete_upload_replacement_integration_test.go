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

func TestCompleteUploadReplacementIntegration(t *testing.T) {
	const userID, entryID, originalRevision, objectID = 7, 31, 6, 25
	const newSize = 11
	for _, scenario := range []struct {
		name           string
		multipart      bool
		missingHistory bool
		skipSettlement bool
		changeReadback bool
		changeSQL      string
		wantError      error
	}{
		{name: "single completion keeps its published revision"},
		{name: "multipart completion keeps its published revision", multipart: true},
		{name: "missing history", missingHistory: true, wantError: domain.ErrUploadTaskBusy},
		{name: "reservation not settled", skipSettlement: true, wantError: domain.ErrInvalidUploadTaskData},
		{name: "expired verification lease", changeSQL: "UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6) WHERE id=19", wantError: domain.ErrUploadTaskBusy},
		{name: "result entry changed", changeSQL: "UPDATE file_entries SET revision=8 WHERE id=31", wantError: domain.ErrUploadTaskBusy},
		{name: "selected object digest changed", changeSQL: "UPDATE stored_objects SET verified_sha256=UNHEX(REPEAT('ff',32)) WHERE id=25", wantError: domain.ErrUploadTaskBusy},
		{name: "task binding changed", changeSQL: "UPDATE upload_tasks SET replacement_expected_revision=5 WHERE id=19", wantError: domain.ErrInvalidUploadTaskData},
		{name: "readback mismatch rolls back", changeReadback: true, wantError: domain.ErrInvalidUploadTaskData},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 准备两种上传方式和具有有效租约的替换任务。
			pool, original := uploadTaskDatabase(t)
			ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
			defer cancel()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}
			execute := func(statement string, arguments ...any) {
				t.Helper()
				if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
					t.Fatal("准备完成快照夹具失败", err)
				}
			}
			digest := domain.UploadContentDigest{SizeBytes: newSize, SHA256: sha256.Sum256([]byte("replacement")), DetectedMIMEType: "text/plain"}
			mode := "SINGLE"
			var session, partSize, partCount any
			if scenario.multipart {
				mode, session, partSize, partCount = "MULTIPART", "replacement-session ", domain.MaxSingleUploadSizeBytes, 2
				digest.SizeBytes = domain.MaxSingleUploadSizeBytes + 1
			}
			execute("UPDATE users SET used_storage=?,reserved_storage=? WHERE id=7", original.FileSize, digest.SizeBytes)
			execute("UPDATE file_entries SET revision=6,source_device_id=NULL WHERE id=31")
			execute(`UPDATE stored_objects SET status='VERIFYING',file_size=?,storage_version_id='replacement-version',
				storage_etag='replacement-etag' WHERE id=23`, digest.SizeBytes)
			execute(`INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size,status,verified_sha256,verified_at)
				VALUES(25,7,'local_minio','fixture-bucket','replacement-ready',?,'READY',?,UTC_TIMESTAMP(6))`, digest.SizeBytes, digest.SHA256[:])
			execute(`UPDATE upload_tasks SET status='VERIFYING',upload_mode=?,storage_upload_id=?,part_size=?,part_count=?,
				file_size=?,reserved_bytes=?,expected_sha256=?,replacement_entry_id=31,replacement_expected_revision=6,
				published_object_id=NULL,result_entry_id=NULL,resolved_name=NULL,completed_at=NULL,
				lease_owner='replacement-worker',lease_version=3,lease_until=UTC_TIMESTAMP(6)+INTERVAL 1 MINUTE,
				expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 DAY,updated_at=UTC_TIMESTAMP(6) WHERE id=19`,
				mode, session, partSize, partCount, digest.SizeBytes, digest.SizeBytes, digest.SHA256[:])
			if scenario.changeReadback {
				execute(`CREATE TRIGGER fixture_alters_replacement_snapshot BEFORE UPDATE ON upload_tasks FOR EACH ROW
					BEGIN
						IF NEW.status='COMPLETED' THEN SET NEW.resolved_name='fixture wrong name'; END IF;
					END`)
			}
			readTask := func() *domain.UploadTask {
				t.Helper()
				task, err := store.FindUserUploadTask(ctx, userID, 19)
				if err != nil {
					t.Fatal(err)
				}
				return task
			}
			task := readTask()
			staged, err := store.FindUserStoredObject(ctx, userID, 23)
			if err != nil {
				t.Fatal(err)
			}
			readState := func() string {
				t.Helper()
				var state string
				if err := pool.QueryRowContext(ctx, `SELECT JSON_ARRAY(e.object_id,e.revision,e.file_size,e.mime_type,e.updated_at,
					u.used_storage,u.reserved_storage,u.change_seq,(SELECT COUNT(*) FROM file_versions),(SELECT COUNT(*) FROM change_events))
					FROM users u JOIN file_entries e ON e.user_id=u.id WHERE u.id=7 AND e.id=31`).Scan(&state); err != nil {
					t.Fatal(err)
				}
				return state
			}
			before := readState()

			// 2. 在真实发布事务内先写引用及账目，再写完成记录。
			var updated *domain.FileEntry
			var completed *domain.UploadTask
			err = store.WithUploadVerificationTransaction(ctx, task, staged, digest,
				func(txContext context.Context, txStore *Store, current *domain.UploadTask, _ *domain.StoredObject) error {
					var err error
					if scenario.missingHistory || scenario.skipSettlement {
						updated, err = txStore.ReplaceUploadFileContent(txContext, entryID, originalRevision, objectID, digest)
						if err == nil && !scenario.skipSettlement {
							err = txStore.SettlePublishedUploadQuota(txContext, current)
						}
					} else {
						updated, err = txStore.ApplyUploadReplacementContent(txContext, current, objectID, digest, domain.DefaultFileVersionRetentionDays)
					}
					if err != nil {
						return err
					}
					if scenario.changeSQL != "" {
						if result := txStore.database.WithContext(txContext).Exec(scenario.changeSQL); result.Error != nil {
							t.Fatal("完成前的故障夹具未成功执行", result.Error)
						}
					}
					completed, err = txStore.CompleteUploadReplacement(txContext, current, objectID, updated)
					return err
				})
			if !errors.Is(err, scenario.wantError) {
				t.Fatal("任务完成结果错误", err)
			}
			if scenario.wantError != nil {
				if completed != nil || !reflect.DeepEqual(task, readTask()) || readState() != before {
					t.Fatal("完成失败留下了任务快照或业务写入", completed)
				}
				return
			}

			// 3. 成功时冻结原条目 ID 和替换版本。后续改名、升版与重复完成均不改写快照。
			if completed == nil || completed.Status != domain.UploadStatusCompleted || completed.ResultEntryID == nil ||
				*completed.ResultEntryID != entryID || completed.PublishedObjectID == nil || *completed.PublishedObjectID != objectID ||
				completed.ReplacementPublishedRevision == nil || *completed.ReplacementPublishedRevision != originalRevision+1 ||
				completed.ReplacementConflictingRevision != nil || completed.ResolvedName == nil || *completed.ResolvedName != updated.Name ||
				completed.ReservedBytes != 0 || completed.LeaseOwner != nil || completed.LeaseUntil != nil || completed.CompletedAt == nil ||
				!reflect.DeepEqual(completed, readTask()) {
				t.Fatal("提交的完成快照不符", completed)
			}
			execute("UPDATE file_entries SET name='later name',name_key=X'6C61746572206E616D65',revision=8 WHERE id=31")
			err = store.withActiveUserTransaction(ctx, userID, func(txContext context.Context, txStore *Store) error {
				repeated, err := txStore.CompleteUploadReplacement(txContext, task, objectID, updated)
				if repeated != nil {
					t.Fatal("旧执行者重新完成了任务", repeated)
				}
				return err
			})
			if !errors.Is(err, domain.ErrUploadTaskNotReady) || !reflect.DeepEqual(completed, readTask()) {
				t.Fatal("重复完成或文件后续改名改变了快照", err)
			}
		})
	}
}
