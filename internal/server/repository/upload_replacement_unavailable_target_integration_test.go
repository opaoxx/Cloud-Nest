package repository

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestUploadReplacementUnavailableTargetIntegration(t *testing.T) {
	const failureMessage = "fixture rejects cleanup after unavailable replacement target"
	for _, sample := range []struct {
		name, mode                string
		hiddenAncestor, directory bool
		rejectGC                  bool
	}{
		{name: "trashed file", mode: "SINGLE"},
		{name: "active file under trashed ancestor", mode: "MULTIPART", hiddenAncestor: true},
		{name: "target is a directory", mode: "SINGLE", directory: true},
		{name: "orphan failure rolls back settlement", mode: "MULTIPART", hiddenAncestor: true, rejectGC: true},
	} {
		t.Run(sample.name, func(t *testing.T) {
			// 1. 为同账号任务保留一条旧历史、独立预占和当前文件引用。
			pool, store, digest := uploadReplacementPublicationFixture(t, sample.mode)
			ctx := t.Context()
			execute := func(statement string, args ...any) {
				t.Helper()
				if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
					t.Fatal("准备不可用目标夹具失败", err)
				}
			}
			execute("UPDATE users SET used_storage=33554432,reserved_storage=reserved_storage+9 WHERE id=7")
			execute("UPDATE file_entries SET revision=3 WHERE id=31")
			execute(`INSERT INTO file_versions(user_id,entry_id,revision,object_id,file_size,retained_until,created_at)
				VALUES(7,31,1,29,16777216,'2026-12-17 08:00:00.123456','2026-11-17 08:00:00.123456')`)
			execute("UPDATE upload_tasks SET replacement_entry_id=31,replacement_expected_revision=3 WHERE id=19")
			readTask := func() *domain.UploadTask {
				t.Helper()
				task, err := store.FindUserUploadTask(ctx, 7, 19)
				if err != nil {
					t.Fatal(err)
				}
				return task
			}
			readObject := func(id uint64) *domain.StoredObject {
				t.Helper()
				object, err := store.FindUserStoredObject(ctx, 7, id)
				if err != nil {
					t.Fatal(err)
				}
				return object
			}
			staged := readObject(23)
			if sample.name == "trashed file" {
				// 目标仍可见且版本相符时，直接请求不可用收尾也应被拒绝。
				err := store.WithUploadVerificationTransaction(ctx, readTask(), staged, digest,
					func(ctx context.Context, txStore *Store, current *domain.UploadTask, _ *domain.StoredObject) error {
						return txStore.FailUploadForUnavailableTarget(ctx, current)
					})
				if !errors.Is(err, domain.ErrUploadTaskBusy) {
					t.Fatal("仍可用的替换目标被错误结束", err)
				}
			}

			// 2. 模拟任务进入校验后，目标被回收、父链不可见或类型不符。
			switch {
			case sample.directory:
				execute("UPDATE upload_tasks SET replacement_entry_id=13,replacement_expected_revision=1 WHERE id=19")
			case sample.hiddenAncestor:
				execute(`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at)
					VALUES(33,7,13,'hidden',X'68696464656E','DIR','TRASHED',UTC_TIMESTAMP(6))`)
				execute("UPDATE file_entries SET parent_id=33,revision=4 WHERE id=31")
			default:
				execute("UPDATE file_entries SET status='TRASHED',deleted_at=UTC_TIMESTAMP(6),revision=4 WHERE id=31")
			}
			if sample.rejectGC {
				execute(`CREATE TRIGGER fixture_reject_unavailable_target_cleanup BEFORE UPDATE ON stored_objects FOR EACH ROW
					BEGIN
						IF NEW.id=23 AND NEW.status='ORPHANED'
							AND EXISTS (SELECT 1 FROM upload_tasks WHERE id=19 AND status='FAILED'
								AND last_error_code='TARGET_UNAVAILABLE' AND replacement_conflicting_revision IS NULL AND reserved_bytes=0)
							AND EXISTS (SELECT 1 FROM users WHERE id=7 AND reserved_storage=9) THEN
							SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='` + failureMessage + `';
						END IF;
					END`)
			}
			readProtectedState := func() string {
				t.Helper()
				var value string
				if err := pool.QueryRowContext(ctx, `SELECT JSON_ARRAY(e.object_id,e.name,e.parent_id,e.revision,e.status,e.file_size,e.updated_at,
					h.object_id,h.revision,h.file_size,h.retained_until,u.used_storage,u.change_seq,
					(SELECT COUNT(*) FROM file_versions),(SELECT COUNT(*) FROM change_events))
					FROM file_entries e JOIN users u ON u.id=e.user_id
					JOIN file_versions h ON h.entry_id=e.id AND h.user_id=e.user_id WHERE e.id=31 AND h.revision=1`).
					Scan(&value); err != nil {
					t.Fatal("读取受保护的原内容和历史失败", err)
				}
				return value
			}
			readReserved := func() uint64 {
				t.Helper()
				var reserved uint64
				if err := pool.QueryRowContext(ctx, "SELECT reserved_storage FROM users WHERE id=7").Scan(&reserved); err != nil {
					t.Fatal(err)
				}
				return reserved
			}
			task, oldObject, before := readTask(), readObject(29), readProtectedState()
			publisher := service.NewUploadPublicationService(func(ctx context.Context, task *domain.UploadTask, object *domain.StoredObject, measured domain.UploadContentDigest,
				action func(context.Context, service.UploadPublicationTransaction, *domain.UploadTask, *domain.StoredObject) error) error {
				return store.WithUploadVerificationTransaction(ctx, task, object, measured, func(ctx context.Context, txStore *Store, current *domain.UploadTask, staged *domain.StoredObject) error {
					return action(ctx, txStore, current, staged)
				})
			})

			// 3. 验证收尾，及回收标记异常时的完整回滚。
			completed, err := publisher.Publish(ctx, task, staged, digest)
			if sample.rejectGC {
				var databaseError *mysql.MySQLError
				if completed != nil || !errors.As(err, &databaseError) || databaseError.Number != 1644 || databaseError.Message != failureMessage ||
					!reflect.DeepEqual(readTask(), task) || !reflect.DeepEqual(readObject(23), staged) ||
					readReserved() != digest.SizeBytes+9 || readProtectedState() != before {
					t.Fatal("收尾故障没有回滚，或未在账目写回后触发", completed, err)
				}
				execute("DROP TRIGGER fixture_reject_unavailable_target_cleanup")
				completed, err = publisher.Publish(ctx, task, staged, digest)
			}
			if completed != nil || !errors.Is(err, domain.ErrUploadReplacementTargetUnavailable) {
				t.Fatal("不可用的替换目标被发布或错误类型不符", completed, err)
			}

			// 4. 保留原内容及历史，公开文件不可用提示，旧执行者无法重复释放预占。
			failed := readTask()
			data, err := dto.NewUploadTaskData(failed)
			if err != nil || data == nil || failed.Status != domain.UploadStatusFailed || failed.ReservedBytes != 0 ||
				failed.CompletedAt == nil || failed.LeaseOwner != nil || failed.LeaseUntil != nil ||
				failed.LastErrorMessage == nil || *failed.LastErrorMessage != domain.ErrUploadReplacementTargetUnavailable.Error() ||
				data.ResultEntryID != nil || data.Replacement == nil ||
				data.Replacement.ConflictingRevision != nil || data.Replacement.PublishedRevision != nil ||
				data.LastError == nil || data.LastError.Code != domain.UploadTargetUnavailableCode ||
				data.LastError.Message != "目标文件已不可用" {
				t.Fatal("失败任务或公开响应不符", data, err)
			}
			orphaned := readObject(23)
			if orphaned.Status != domain.StoredObjectStatusOrphaned || orphaned.GCAfter == nil || orphaned.UnreferencedAt == nil ||
				!orphaned.GCAfter.Equal(orphaned.UnreferencedAt.Add(7*24*time.Hour)) ||
				!reflect.DeepEqual(readObject(29), oldObject) || readProtectedState() != before || readReserved() != 9 {
				t.Fatal("失败收尾影响了历史、原内容、已用空间或其他预占")
			}
			if repeated, err := publisher.Publish(ctx, task, staged, digest); repeated != nil ||
				!errors.Is(err, domain.ErrUploadTaskNotReady) || !reflect.DeepEqual(readTask(), failed) ||
				!reflect.DeepEqual(readObject(23), orphaned) || readReserved() != 9 {
				t.Fatal("重复收尾改写了任务或预占", repeated, err)
			}
		})
	}
}
