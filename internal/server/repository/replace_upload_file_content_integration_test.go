package repository

import (
	"context"
	"crypto/sha256"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestReplaceUploadFileContentIntegration(t *testing.T) {
	// 1. 准备一个有当前引用的文件及新内容对应的独占对象。
	pool, _ := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	const userID, entryID, revision, newObjectID = 7, 31, 6, 23
	const oldSize, newSize = 16777216, 11
	digest := domain.UploadContentDigest{SizeBytes: newSize, SHA256: sha256.Sum256([]byte("replacement")), DetectedMIMEType: "image/png"}
	execute := func(t *testing.T, statement string, arguments ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
			t.Fatal("准备切换夹具失败", err)
		}
	}
	execute(t, `UPDATE file_entries SET revision=6,mime_type='text/markdown',extension='md',source_device_id=11,source_device_name='fixture',
		content_modified_at='2026-11-17 08:00:00.123456' WHERE id=31`)
	execute(t, "UPDATE users SET used_storage=?,reserved_storage=? WHERE id=7", oldSize, newSize)
	execute(t, "UPDATE stored_objects SET status='READY',file_size=?,verified_sha256=?,verified_at=UTC_TIMESTAMP(6) WHERE id IN (23,24)", newSize, digest.SHA256[:])
	replace := func(expected, objectID uint64, observed domain.UploadContentDigest) (*domain.FileEntry, error) {
		var result *domain.FileEntry
		err := store.withActiveUserTransaction(ctx, userID, func(txContext context.Context, txStore *Store) error {
			var err error
			result, err = txStore.ReplaceUploadFileContent(txContext, entryID, expected, objectID, observed)
			return err
		})
		return result, err
	}
	readState := func(t *testing.T) string {
		t.Helper()
		var state string
		if err := pool.QueryRowContext(ctx, `SELECT JSON_ARRAY(e.object_id,e.file_size,e.mime_type,e.revision,e.modified_at,e.updated_at,
			e.content_modified_at,e.parent_id,e.name,e.created_at,u.used_storage,u.reserved_storage,u.change_seq,t.status,
			(SELECT COUNT(*) FROM file_versions),(SELECT COUNT(*) FROM change_events))
			FROM file_entries e JOIN users u ON u.id=e.user_id JOIN upload_tasks t ON t.user_id=u.id WHERE e.id=31 AND t.id=19`).
			Scan(&state); err != nil {
			t.Fatal("读取切换前后状态失败", err)
		}
		return state
	}
	initial := readState(t)
	original, err := store.FindUserFileEntry(ctx, userID, entryID)
	if err != nil {
		t.Fatal(err)
	}

	// 2. 旧版本及非法的新对象或 MIME 均在写入前拒绝。
	if result, err := store.ReplaceUploadFileContent(ctx, entryID, revision, newObjectID, digest); result != nil || err == nil {
		t.Fatal("事务外切换未拒绝", result, err)
	}
	badSize, badDigest, badMIME := digest, digest, digest
	badSize.SizeBytes++
	badDigest.SHA256[0] ^= 0xff
	badMIME.DetectedMIMEType = "image/png; charset=utf-8"
	for _, sample := range []struct {
		name                       string
		expectedRevision, objectID uint64
		digest                     domain.UploadContentDigest
		want                       error
	}{
		{"stale version", revision - 1, newObjectID, digest, domain.ErrEntryRevisionConflict},
		{"foreign object", revision, 24, digest, domain.ErrStoredObjectNotFound},
		{"missing object", revision, 999, digest, domain.ErrStoredObjectNotFound},
		{"wrong size", revision, newObjectID, badSize, domain.ErrInvalidStoredObjectData},
		{"wrong digest", revision, newObjectID, badDigest, domain.ErrInvalidStoredObjectData},
		{"invalid media type", revision, newObjectID, badMIME, domain.ErrInvalidFileEntryData},
	} {
		t.Run(sample.name, func(t *testing.T) {
			result, err := replace(sample.expectedRevision, sample.objectID, sample.digest)
			if result != nil || !errors.Is(err, sample.want) || readState(t) != initial {
				t.Fatal("拒绝后改变了当前文件或原因不符", result, err)
			}
		})
	}
	execute(t, "UPDATE stored_objects SET status='VERIFYING' WHERE id=23")
	if result, err := replace(revision, newObjectID, digest); result != nil || !errors.Is(err, domain.ErrInvalidStoredObjectData) || readState(t) != initial {
		t.Fatal("未就绪对象被引用", result, err)
	}
	execute(t, "UPDATE stored_objects SET status='READY' WHERE id=23")

	t.Run("readback mismatch and outer rollback", func(t *testing.T) {
		// 3. 写入后的核验失败，以及保存历史后的外层故障，都应完整回滚。
		execute(t, "CREATE TRIGGER fixture_changes_replacement_mime BEFORE UPDATE ON file_entries FOR EACH ROW SET NEW.mime_type='text/plain'")
		result, err := replace(revision, newObjectID, digest)
		execute(t, "DROP TRIGGER fixture_changes_replacement_mime")
		if result != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) || readState(t) != initial {
			t.Fatal("读回错误未使引用切换回滚", result, err)
		}

		rollbackFixture := errors.New("fixture aborts after reference update")
		err = store.withActiveUserTransaction(ctx, userID, func(txContext context.Context, txStore *Store) error {
			saved, err := txStore.SaveCurrentFileVersion(txContext, entryID, revision, domain.DefaultFileVersionRetentionDays)
			if err != nil {
				return err
			}
			if saved.ObjectID != 29 || saved.Revision != revision || saved.FileSize != oldSize {
				t.Fatal("旧引用保存不符", saved)
			}
			updated, err := txStore.ReplaceUploadFileContent(txContext, entryID, revision, newObjectID, digest)
			if err != nil {
				return err
			}
			if updated.Revision != revision+1 || updated.FileSize != newSize {
				t.Fatal("外层回滚前未看到新的引用版本", updated)
			}
			return rollbackFixture
		})
		if !errors.Is(err, rollbackFixture) || readState(t) != initial {
			t.Fatal("外层回滚未撤销历史和当前引用", err)
		}
	})

	t.Run("switches object and increments exactly once", func(t *testing.T) {
		// 4. 提交这一步的隔离写入，核对身份、引用、MIME 和三项 UTC 时间。
		var before, after time.Time
		if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&before); err != nil {
			t.Fatal(err)
		}
		updated, err := replace(revision, newObjectID, digest)
		if err != nil || updated == nil || updated.ID != entryID || updated.UserID != userID || updated.Name != original.Name ||
			updated.ParentID == nil || *updated.ParentID != *original.ParentID || !updated.CreatedAt.Equal(original.CreatedAt) ||
			updated.Revision != revision+1 || updated.FileSize != newSize {
			t.Fatal("切换后文件快照不符", updated, err)
		}
		if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&after); err != nil {
			t.Fatal(err)
		}
		content, err := store.FindUserFileEntryContent(ctx, userID, entryID)
		if err != nil || content == nil || content.ObjectID == nil || *content.ObjectID != newObjectID ||
			content.MIMEType == nil || *content.MIMEType != digest.DetectedMIMEType {
			t.Fatal("切换的对象或 MIME 未持久化", content, err)
		}
		display, err := store.FindUserFileEntryDisplay(ctx, userID, entryID)
		if err != nil || display == nil || display.ContentModifiedAt == nil ||
			!display.ContentModifiedAt.Equal(updated.ModifiedAt) || !updated.UpdatedAt.Equal(updated.ModifiedAt) ||
			updated.ModifiedAt.Before(before) || updated.ModifiedAt.After(after) ||
			display.Extension == nil || *display.Extension != "md" || display.SourceDeviceID == nil || *display.SourceDeviceID != 11 {
			t.Fatal("时间或原有展示信息不符", display, err)
		}
		firstCommit := readState(t)
		repeated, err := replace(revision, newObjectID, digest)
		var conflict *domain.EntryRevisionConflictError
		if repeated != nil || !errors.As(err, &conflict) || conflict.CurrentRevision != revision+1 || readState(t) != firstCommit {
			t.Fatal("旧版本重复调用再次改变了文件", repeated, err)
		}
	})

	t.Run("zero bytes unknown MIME and maximum revision", func(t *testing.T) {
		// 5. 零字节有效，未知 MIME 清为 NULL，最后一次递增可以到达 uint64 上限。
		empty := domain.UploadContentDigest{SHA256: sha256.Sum256(nil)}
		execute(t, `INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size,status,verified_sha256,verified_at)
			VALUES(25,7,'local_minio','fixture-bucket','replacement-empty',0,'READY',?,UTC_TIMESTAMP(6))`, empty.SHA256[:])
		lastExpected := uint64(math.MaxUint64 - 1)
		execute(t, "UPDATE file_entries SET revision=? WHERE id=31", lastExpected)
		updated, err := replace(lastExpected, 25, empty)
		if err != nil || updated == nil || updated.Revision != math.MaxUint64 || updated.FileSize != 0 {
			t.Fatal("空文件或最后一次版本递增失败", updated, err)
		}
		content, err := store.FindUserFileEntryContent(ctx, userID, entryID)
		if err != nil || content == nil || content.ObjectID == nil || *content.ObjectID != 25 || content.MIMEType != nil {
			t.Fatal("未知 MIME 未清为空，或空文件对象不符", content, err)
		}
		repeated, err := replace(math.MaxUint64, 25, empty)
		if repeated != nil || !errors.Is(err, domain.ErrEntryRevisionExhausted) {
			t.Fatal("版本上限未拒绝后续替换", repeated, err)
		}
	})

	// 6. 仓储写入不结算配额、任务或变更，也不修改原不可变对象。
	var used, reserved, sequence, eventCount, versions uint64
	var status string
	if err := pool.QueryRowContext(ctx, `SELECT u.used_storage,u.reserved_storage,u.change_seq,t.status,
		(SELECT COUNT(*) FROM change_events),(SELECT COUNT(*) FROM file_versions)
		FROM users u JOIN upload_tasks t ON t.user_id=u.id WHERE u.id=7 AND t.id=19`).
		Scan(&used, &reserved, &sequence, &status, &eventCount, &versions); err != nil ||
		used != oldSize || reserved != newSize || sequence != 0 || status != "COMPLETED" || eventCount != 0 || versions != 0 {
		t.Fatal("引用切换误结算了其他资源", used, reserved, sequence, status, eventCount, versions, err)
	}
	oldObject, err := store.FindUserStoredObject(ctx, userID, 29)
	if err != nil || oldObject == nil || oldObject.Status != domain.StoredObjectStatusReady || oldObject.FileSize != oldSize ||
		!reflect.DeepEqual(oldObject.VerifiedSHA256, new([sha256.Size]byte)) || oldObject.GCAfter != nil {
		t.Fatal("原对象被改写", oldObject, err)
	}
}
