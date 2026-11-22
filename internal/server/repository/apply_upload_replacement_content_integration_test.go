package repository

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestApplyUploadReplacementContentIntegration(t *testing.T) {
	const userID, entryID, originalRevision, selectedObjectID = 7, 31, 6, 25
	const otherUsed, otherReserved = 17, 9
	const quotaFailureMessage = "fixture stops after replacement file and history writes"

	for _, scenario := range []struct {
		name              string
		newSize           uint64
		changedBinding    bool
		shortReserve      bool
		quotaWriteFailure bool
		wantError         error
	}{
		{name: "single upload retains old usage", newSize: 11},
		{name: "multipart upload retains old usage", newSize: domain.MaxSingleUploadSizeBytes + 1},
		{name: "zero byte upload retains old usage"},
		{name: "stale task binding", newSize: 11, changedBinding: true, wantError: domain.ErrInvalidUploadTaskData},
		{name: "missing reserved bytes", newSize: 11, shortReserve: true, wantError: domain.ErrInvalidAccountData},
		{name: "quota error rolls back both content writes", newSize: 11, quotaWriteFailure: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 准备当前文件、已验证的发布对象、替换任务及其他用量。
			pool, original := uploadTaskDatabase(t)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}
			execute := func(statement string, arguments ...any) {
				t.Helper()
				if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
					t.Fatal("准备替换结算夹具失败", err)
				}
			}
			reserved := scenario.newSize + otherReserved
			if scenario.shortReserve {
				reserved = scenario.newSize - 1
			}
			used := original.FileSize + otherUsed
			digest := domain.UploadContentDigest{SizeBytes: scenario.newSize, SHA256: sha256.Sum256([]byte("replacement")), DetectedMIMEType: "application/octet-stream"}
			if scenario.newSize == 0 {
				digest.SHA256 = sha256.Sum256(nil)
			}
			mode := "SINGLE"
			var session, partSize, partCount any
			if scenario.newSize > domain.MaxSingleUploadSizeBytes {
				mode, session, partSize, partCount = "MULTIPART", "replacement-session", domain.MaxSingleUploadSizeBytes, 2
			}
			execute("UPDATE users SET used_storage=?,reserved_storage=? WHERE id=7", used, reserved)
			execute("UPDATE file_entries SET revision=6 WHERE id=31")
			execute(`UPDATE stored_objects SET status='VERIFYING',file_size=?,storage_version_id='replacement-version',
				storage_etag='replacement-etag' WHERE id=23`, scenario.newSize)
			execute(`INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size,status,verified_sha256,verified_at)
				VALUES(25,7,'local_minio','fixture-bucket','replacement-ready',?,'READY',?,UTC_TIMESTAMP(6))`, scenario.newSize, digest.SHA256[:])
			execute(`UPDATE upload_tasks SET status='VERIFYING',upload_mode=?,storage_upload_id=?,part_size=?,part_count=?,
				file_size=?,reserved_bytes=?,expected_sha256=?,replacement_entry_id=31,replacement_expected_revision=6,
				published_object_id=NULL,result_entry_id=NULL,resolved_name=NULL,completed_at=NULL,
				lease_owner='replacement-worker',lease_version=3,lease_until=UTC_TIMESTAMP(6)+INTERVAL 1 MINUTE,
				expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 DAY,updated_at=UTC_TIMESTAMP(6) WHERE id=19`,
				mode, session, partSize, partCount, scenario.newSize, scenario.newSize, digest.SHA256[:])
			if scenario.quotaWriteFailure {
				// 仅在前两项写入已经可见、配额正要增加时触发错误。
				execute(`CREATE TRIGGER fixture_reject_replacement_quota BEFORE UPDATE ON users FOR EACH ROW
					BEGIN
						IF NEW.id=7 AND NEW.used_storage=OLD.used_storage+11
							AND EXISTS (SELECT 1 FROM file_versions WHERE user_id=7 AND entry_id=31 AND revision=6 AND object_id=29)
							AND EXISTS (SELECT 1 FROM file_entries WHERE user_id=7 AND id=31 AND revision=7 AND object_id=25) THEN
							SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='` + quotaFailureMessage + `';
						END IF;
					END`)
			}
			task, err := store.FindUserUploadTask(ctx, userID, 19)
			if err != nil {
				t.Fatal(err)
			}
			staged, err := store.FindUserStoredObject(ctx, userID, 23)
			if err != nil {
				t.Fatal(err)
			}
			readState := func() string {
				t.Helper()
				var state string
				if err := pool.QueryRowContext(ctx, `SELECT JSON_ARRAY(e.object_id,e.revision,e.file_size,e.mime_type,e.updated_at,
					u.used_storage,u.reserved_storage,u.change_seq,t.status,t.reserved_bytes,t.lease_version,t.updated_at,
					(SELECT COUNT(*) FROM file_versions),(SELECT COUNT(*) FROM change_events))
					FROM users u JOIN file_entries e ON e.user_id=u.id JOIN upload_tasks t ON t.user_id=u.id
					WHERE u.id=7 AND e.id=31 AND t.id=19`).Scan(&state); err != nil {
					t.Fatal(err)
				}
				return state
			}
			before := readState()

			// 2. 通过真实写回复核事务执行组合入口。
			var updated *domain.FileEntry
			err = store.WithUploadVerificationTransaction(ctx, task, staged, digest,
				func(txContext context.Context, txStore *Store, current *domain.UploadTask, _ *domain.StoredObject) error {
					if scenario.changedBinding {
						// 使用收到的快照副本模拟过期绑定，数据库记录仍取锁内版本。
						current.ReplacementExpectedRevision = new(uint64(originalRevision - 1))
					}
					var err error
					updated, err = txStore.ApplyUploadReplacementContent(txContext, current, selectedObjectID, digest, domain.DefaultFileVersionRetentionDays)
					return err
				})
			if scenario.quotaWriteFailure {
				var mysqlError *mysql.MySQLError
				if !errors.As(err, &mysqlError) || mysqlError.Number != 1644 || mysqlError.Message != quotaFailureMessage {
					t.Fatal("未在历史及文件写入后触发预期的配额故障", err)
				}
			} else if !errors.Is(err, scenario.wantError) {
				t.Fatal("替换结算结果错误", err)
			}
			if scenario.wantError != nil || scenario.quotaWriteFailure {
				if updated != nil || readState() != before {
					t.Fatal("失败结算留下了历史、文件或配额变更", updated)
				}
				return
			}

			// 3. 核对历史旧对象、当前引用、账目和任务预占的实际提交值。
			if updated == nil || updated.ID != entryID || updated.Revision != originalRevision+1 || updated.FileSize != scenario.newSize {
				t.Fatal("未返回更新后的原文件", updated)
			}
			var oldObject, oldBytes, storedRevision, currentObject, currentBytes, storedUsed, storedReserved, taskReserved, sequence, events uint64
			var status string
			if err := pool.QueryRowContext(ctx, `SELECT h.object_id,h.file_size,h.revision,e.object_id,e.file_size,
				u.used_storage,u.reserved_storage,t.reserved_bytes,u.change_seq,t.status,(SELECT COUNT(*) FROM change_events)
				FROM file_versions h JOIN file_entries e ON e.user_id=h.user_id AND e.id=h.entry_id
				JOIN users u ON u.id=h.user_id JOIN upload_tasks t ON t.user_id=u.id
				WHERE u.id=7 AND h.entry_id=31 AND t.id=19`).
				Scan(&oldObject, &oldBytes, &storedRevision, &currentObject, &currentBytes,
					&storedUsed, &storedReserved, &taskReserved, &sequence, &status, &events); err != nil ||
				oldObject != 29 || oldBytes != original.FileSize || storedRevision != originalRevision ||
				currentObject != selectedObjectID || currentBytes != scenario.newSize ||
				storedUsed != used+scenario.newSize || storedReserved != otherReserved || taskReserved != 0 ||
				status != "VERIFYING" || sequence != 0 || events != 0 {
				t.Fatal("已用、预占、历史、当前引用或任务提交值不符", oldBytes, currentBytes, storedUsed, storedReserved, taskReserved, status, err)
			}
			after := readState()
			err = store.withActiveUserTransaction(ctx, userID, func(txContext context.Context, txStore *Store) error {
				repeated, err := txStore.ApplyUploadReplacementContent(txContext, task, selectedObjectID, digest, domain.DefaultFileVersionRetentionDays)
				if repeated != nil || err == nil {
					t.Fatal("同一已结算任务再次成功切换", repeated)
				}
				return err
			})
			if err == nil || readState() != after {
				t.Fatal("重复处理再次记账或改写记录", err)
			}
		})
	}
}
