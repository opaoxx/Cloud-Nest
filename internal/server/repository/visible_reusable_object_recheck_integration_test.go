package repository

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestRecheckVisibleReusableObjectIntegration(t *testing.T) {
	ctx, pool, store := prepareMultipartCompletionDatabase(t)
	actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
	otherActor := domain.Actor{UserID: 8, DeviceID: 12, SessionID: 22}
	const objectID, fileID, directoryID, rootID = 29, 31, 47, 13
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	execute("INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(47,7,13,'folder',X'666F6C646572','DIR')")
	execute("UPDATE file_entries SET parent_id=? WHERE id=?", directoryID, fileID)
	execute("UPDATE stored_objects SET storage_version_id='fixture-source-version' WHERE id=?", objectID)
	candidate, err := store.FindUserVisibleReusableObject(ctx, actor.UserID, [32]byte{}, domain.MaxSingleUploadSizeBytes)
	if err != nil || candidate == nil || candidate.ID != objectID {
		t.Fatal("未取得可见秒传候选", candidate, err)
	}
	if result, err := store.RecheckVisibleReusableObject(ctx, candidate); result != nil || err == nil {
		t.Fatal("事务外入口不应交付锁定候选", result, err)
	}

	// 1. 验证正常复核结果与锁的范围，并确认其他账号获取不到它。
	if err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
		locked, err := txStore.RecheckVisibleReusableObject(txContext, candidate)
		if err != nil || !reflect.DeepEqual(locked, candidate) {
			return fmt.Errorf("锁内未取得同一秒传对象: %v", err)
		}
		contender, err := pool.BeginTx(txContext, nil)
		if err != nil {
			return err
		}
		defer contender.Rollback()
		for _, lock := range []struct {
			query string
			id    uint64
		}{
			{"SELECT id FROM users WHERE id=? FOR UPDATE NOWAIT", actor.UserID},
			{"SELECT id FROM stored_objects WHERE id=? FOR UPDATE NOWAIT", candidate.ID},
		} {
			var id uint64
			err := contender.QueryRowContext(txContext, lock.query, lock.id).Scan(&id)
			var lockError *mysql.MySQLError
			if !errors.As(err, &lockError) || lockError.Number != 3572 {
				return fmt.Errorf("外层事务没有保持账号或对象行锁: %v", err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	checkNoMatch := func(checkActor domain.Actor) {
		t.Helper()
		err := store.WithUserTransaction(ctx, checkActor, func(txContext context.Context, txStore *Store) error {
			object, err := txStore.RecheckVisibleReusableObject(txContext, candidate)
			if err != nil {
				return err
			}
			if object != nil {
				return fmt.Errorf("旧秒传候选仍被交付")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	checkNoMatch(otherActor)
	after, err := store.FindUserStoredObject(ctx, actor.UserID, candidate.ID)
	if err != nil || !reflect.DeepEqual(after, candidate) {
		t.Fatal("加锁复核改变了对象", after, err)
	}

	// 2. 使用正式回收写入隐藏祖先目录，原文件仍活动也不得复用。
	if err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
		_, err := txStore.TrashUserFileEntry(txContext, directoryID, 1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	entry, err := store.FindUserFileEntry(ctx, actor.UserID, fileID)
	if err != nil || entry.Status != "ACTIVE" {
		t.Fatal("夹具未保留原文件的活动状态", entry, err)
	}
	checkNoMatch(actor)

	// 3. 重新暴露文件后模拟存储版本变化。再提交真实 GC 删除意图，验证旧候选失效。
	execute("UPDATE file_entries SET parent_id=? WHERE id=?", rootID, fileID)
	execute("UPDATE stored_objects SET storage_version_id='fixture-replacement-version' WHERE id=?", objectID)
	visible, err := store.FindUserVisibleReusableObject(ctx, actor.UserID, *candidate.VerifiedSHA256, candidate.FileSize)
	if err != nil || visible == nil || visible.StorageVersionID == nil || *visible.StorageVersionID != "fixture-replacement-version" {
		t.Fatal("版本变化夹具没有保持内容可见", visible, err)
	}
	checkNoMatch(actor)
	execute("UPDATE stored_objects SET storage_version_id='fixture-source-version' WHERE id=?", objectID)
	execute("UPDATE file_entries SET status='PURGED',object_id=NULL,purged_at=UTC_TIMESTAMP(6) WHERE id=?", fileID)
	execute(`UPDATE stored_objects SET unreferenced_at=UTC_TIMESTAMP(6)-INTERVAL 8 DAY,
		gc_after=UTC_TIMESTAMP(6)-INTERVAL 1 DAY WHERE id=?`, objectID)
	gcCandidates, err := store.ListStoredObjectGCCandidates(ctx, 1)
	if err != nil || len(gcCandidates) != 1 || gcCandidates[0].ObjectID != objectID {
		t.Fatal("未取得预期的清理候选", gcCandidates, err)
	}
	claimed, err := store.ClaimStoredObjectGC(ctx, gcCandidates[0])
	if err != nil || claimed == nil || claimed.Status != domain.StoredObjectStatusDeleting {
		t.Fatal("未提交专用测试对象的删除意图", claimed, err)
	}
	checkNoMatch(actor)
	t.Log("账号与对象锁在外层事务内有效，隐藏祖先、版本变化和 GC 领取均使旧候选失效")
}
