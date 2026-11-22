package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestUploadReplacementPublicationIntegration(t *testing.T) {
	const eventFailureMessage = "fixture rejects event after replacement writes"
	const userID, taskID, entryID, stagedID, oldObjectID, reusedObjectID = 7, 19, 31, 23, 29, 25
	for _, scenario := range []struct {
		name, mode         string
		reuse, rejectEvent bool
		staleTarget        bool
	}{
		{name: "publish staged object", mode: "SINGLE"},
		{name: "publish reusable object", mode: "MULTIPART", reuse: true},
		{name: "failed event rolls back promoted object", mode: "SINGLE", rejectEvent: true},
		{name: "failed event restores both GC timers", mode: "MULTIPART", reuse: true, rejectEvent: true},
		{name: "stale target keeps current content", mode: "SINGLE", staleTarget: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 建立同账号的替换目标、预占、任务和已核对的暂存对象。
			pool, store, digest := uploadReplacementPublicationFixture(t, scenario.mode)
			ctx := t.Context()
			execute := func(statement string, arguments ...any) {
				t.Helper()
				if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
					t.Fatal("准备替换发布夹具失败", err)
				}
			}
			execute("UPDATE upload_tasks SET replacement_entry_id=31,replacement_expected_revision=1 WHERE id=19")
			selectedID := uint64(stagedID)
			objectIDs := []uint64{stagedID, oldObjectID}
			if scenario.reuse {
				selectedID = reusedObjectID
				objectIDs = append(objectIDs, selectedID)
				execute(`INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size,status,
					verified_sha256,verified_at,unreferenced_at,gc_after)
					VALUES(25,7,'local_minio','fixture-bucket','replacement-ready',?,'READY',?,UTC_TIMESTAMP(6),
						UTC_TIMESTAMP(6)-INTERVAL 1 DAY,UTC_TIMESTAMP(6)+INTERVAL 6 DAY)`, digest.SizeBytes, digest.SHA256[:])
			}
			if scenario.staleTarget {
				execute("UPDATE file_entries SET revision=2 WHERE id=31")
			}
			if scenario.rejectEvent {
				// 只有历史、文件、配额、任务、序号和对象状态均已改变时才触发。
				execute(`CREATE TRIGGER fixture_reject_replacement_event BEFORE INSERT ON change_events FOR EACH ROW
					BEGIN
						IF NEW.user_id=7 AND NEW.sequence=1 AND NEW.event_type='FILE_CONTENT_REPLACED'
							AND EXISTS (SELECT 1 FROM file_versions WHERE user_id=7 AND entry_id=31 AND revision=1 AND object_id=29)
							AND EXISTS (SELECT 1 FROM users WHERE id=7 AND change_seq=1 AND reserved_storage=0
								AND used_storage=16777216+(SELECT file_size FROM file_entries WHERE id=31))
							AND EXISTS (SELECT 1 FROM upload_tasks t JOIN file_entries e ON e.id=t.result_entry_id AND e.user_id=t.user_id
								JOIN stored_objects p ON p.id=e.object_id JOIN stored_objects s ON s.id=t.staged_object_id
								WHERE t.id=19 AND t.status='COMPLETED' AND t.replacement_published_revision=e.revision
									AND e.id=31 AND e.revision=2 AND e.object_id=t.published_object_id AND p.status='READY'
									AND p.unreferenced_at IS NULL AND p.gc_after IS NULL
									AND (p.id=s.id OR (s.status='ORPHANED' AND s.gc_after IS NOT NULL))) THEN
							SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='` + eventFailureMessage + `';
						END IF;
					END`)
			}
			readTask := func() *domain.UploadTask {
				t.Helper()
				task, err := store.FindUserUploadTask(ctx, userID, taskID)
				if err != nil {
					t.Fatal(err)
				}
				return task
			}
			readObjects := func() []*domain.StoredObject {
				t.Helper()
				objects := make([]*domain.StoredObject, 0, len(objectIDs))
				for _, id := range objectIDs {
					object, err := store.FindUserStoredObject(ctx, userID, id)
					if err != nil {
						t.Fatal(err)
					}
					objects = append(objects, object)
				}
				return objects
			}
			readState := func() string {
				t.Helper()
				var state string
				if err := pool.QueryRowContext(ctx, `SELECT JSON_ARRAY(e.object_id,e.revision,e.file_size,e.name,e.parent_id,e.mime_type,
					e.modified_at,e.updated_at,e.content_modified_at,u.used_storage,u.reserved_storage,u.change_seq,
					(SELECT COUNT(*) FROM file_entries WHERE user_id=7 AND entry_type='FILE'),
					(SELECT COUNT(*) FROM file_versions),(SELECT COUNT(*) FROM change_events))
					FROM users u JOIN file_entries e ON e.user_id=u.id WHERE u.id=7 AND e.id=31`).Scan(&state); err != nil {
					t.Fatal(err)
				}
				return state
			}
			task, objects, before := readTask(), readObjects(), readState()
			checkUnchanged := func() {
				t.Helper()
				if readState() != before || !reflect.DeepEqual(readTask(), task) || !reflect.DeepEqual(readObjects(), objects) {
					t.Fatal("失败发布留下了文件、配额、历史、任务、对象或日志变化")
				}
			}

			// 2. 调用后台共用发布入口，检查业务拒绝或事件写入失败的回滚。
			publisher := service.NewUploadPublicationService(func(ctx context.Context, task *domain.UploadTask, staged *domain.StoredObject, measured domain.UploadContentDigest,
				action func(context.Context, service.UploadPublicationTransaction, *domain.UploadTask, *domain.StoredObject) error) error {
				return store.WithUploadVerificationTransaction(ctx, task, staged, measured, func(ctx context.Context, txStore *Store, current *domain.UploadTask, object *domain.StoredObject) error {
					return action(ctx, txStore, current, object)
				})
			})
			completed, err := publisher.Publish(ctx, task, objects[0], digest)
			if scenario.staleTarget {
				var conflict *domain.EntryRevisionConflictError
				if completed != nil || !errors.As(err, &conflict) || conflict.CurrentRevision != 2 {
					t.Fatal("旧版本被误发布或没有返回当前版本", completed, err)
				}
				checkUnchanged()
				return
			}
			if scenario.rejectEvent {
				var databaseError *mysql.MySQLError
				if completed != nil || !errors.As(err, &databaseError) || databaseError.Number != 1644 || databaseError.Message != eventFailureMessage {
					t.Fatal("未在各项写入后触发事件故障", completed, err)
				}
				checkUnchanged()
				execute("DROP TRIGGER fixture_reject_replacement_event")
				completed, err = publisher.Publish(ctx, task, objects[0], digest)
			}

			// 3. 核对完成记录、历史、配额、对象保护和事件，并验证重复处理。
			if err != nil || completed == nil || completed.Status != domain.UploadStatusCompleted ||
				completed.ResultEntryID == nil || *completed.ResultEntryID != entryID ||
				completed.ReplacementPublishedRevision == nil || *completed.ReplacementPublishedRevision != 2 ||
				completed.PublishedObjectID == nil || *completed.PublishedObjectID != selectedID ||
				!reflect.DeepEqual(completed, readTask()) {
				t.Fatal("替换完成记录未正确提交", completed, err)
			}
			var oldID, oldSize, currentID, currentSize, used, reserved, sequence, files uint64
			if err := pool.QueryRowContext(ctx, `SELECT h.object_id,h.file_size,e.object_id,e.file_size,u.used_storage,
				u.reserved_storage,u.change_seq,(SELECT COUNT(*) FROM file_entries WHERE user_id=7 AND entry_type='FILE')
				FROM file_versions h JOIN file_entries e ON e.user_id=h.user_id AND e.id=h.entry_id
				JOIN users u ON u.id=e.user_id WHERE h.entry_id=31 AND h.revision=1`).
				Scan(&oldID, &oldSize, &currentID, &currentSize, &used, &reserved, &sequence, &files); err != nil ||
				oldID != oldObjectID || oldSize != domain.MaxSingleUploadSizeBytes || currentID != selectedID ||
				currentSize != digest.SizeBytes || used != oldSize+digest.SizeBytes || reserved != 0 || sequence != 1 || files != 1 {
				t.Fatal("历史、当前引用或配额提交不符", oldID, oldSize, currentID, currentSize, used, reserved, sequence, files, err)
			}
			savedObjects := readObjects()
			published := savedObjects[0]
			if scenario.reuse {
				published = savedObjects[2]
				if savedObjects[0].Status != domain.StoredObjectStatusOrphaned || savedObjects[0].GCAfter == nil {
					t.Fatal("冗余暂存对象未进入 GC", savedObjects[0])
				}
			}
			if published.Status != domain.StoredObjectStatusReady || published.GCAfter != nil || !reflect.DeepEqual(savedObjects[1], objects[1]) {
				t.Fatal("发布对象或历史对象的保护状态不符", published, savedObjects[1])
			}
			var eventEntry, eventRevision uint64
			var eventType string
			var rawPayload []byte
			var payload map[string]any
			wantPayload := map[string]any{"entry_id": "31", "parent_id": "13", "name": "发布笔记.MD", "revision": "2",
				"size_bytes": float64(digest.SizeBytes), "sha256": fmt.Sprintf("%x", digest.SHA256)}
			if err := pool.QueryRowContext(ctx, "SELECT entry_id,revision,event_type,payload FROM change_events WHERE user_id=7 AND `sequence`=1").
				Scan(&eventEntry, &eventRevision, &eventType, &rawPayload); err != nil || eventEntry != entryID ||
				eventRevision != 2 || eventType != domain.FileContentReplacedEventType ||
				json.Unmarshal(rawPayload, &payload) != nil || !reflect.DeepEqual(payload, wantPayload) {
				t.Fatal("替换事件不符", eventType, string(rawPayload), err)
			}
			after := readState()
			if repeated, err := publisher.Publish(ctx, task, objects[0], digest); repeated != nil ||
				!errors.Is(err, domain.ErrUploadTaskNotReady) || readState() != after {
				t.Fatal("重复发布改变了已提交的文件、历史或事件", repeated, err)
			}
		})
	}
}
