package repository

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm/clause"
)

func TestCheckStoredObjectUnreferencedIntegration(t *testing.T) {
	// 1. 复用正式上传夹具，把发布文件放入正在清理的目录。
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	pool, _ := uploadTaskDatabase(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,purge_started_at)
		  VALUES (32,7,13,'待清理目录',CAST('待清理目录' AS BINARY),'DIR','PURGING',UTC_TIMESTAMP(6))`,
		"UPDATE file_entries SET parent_id=32 WHERE user_id=7 AND id=31",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("准备对象引用夹具失败", err)
		}
	}
	roots, err := store.ListPurgeRootCandidates(ctx, 1)
	if err != nil || len(roots) != 1 || roots[0].EntryID != 32 {
		t.Fatal("未读取到待清理根项", roots, err)
	}
	ready, err := store.FindUserStoredObject(ctx, 7, 29)
	if err != nil || ready.Status != domain.StoredObjectStatusReady {
		t.Fatal("未取得 READY 对象", ready, err)
	}
	foreign, err := store.FindUserStoredObject(ctx, 8, 24)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.checkStoredObjectUnreferenced(ctx, ready); err == nil {
		t.Fatal("普通 Store 不应检查对象的当前引用")
	}
	lockObject := func(txContext context.Context, txStore *Store) (*domain.StoredObject, error) {
		lockingStore := *txStore
		lockingStore.database = txStore.database.WithContext(txContext).Clauses(clause.Locking{Strength: "UPDATE"})
		return lockingStore.FindUserStoredObject(txContext, ready.OwnerUserID, ready.ID)
	}

	// 2. 复核仍在清理子树中的文件，随后解除引用并检查同事务内的变化。
	err = store.WithPurgeRootTransaction(ctx, roots[0], func(txContext context.Context, txStore *Store) error {
		object, err := lockObject(txContext, txStore)
		if err != nil {
			return err
		}
		if err := txStore.checkStoredObjectUnreferenced(txContext, foreign); !errors.Is(err, domain.ErrInvalidStoredObjectData) {
			return fmt.Errorf("跨账号对象应被拒绝：%v", err)
		}
		if err := txStore.checkStoredObjectUnreferenced(txContext, object); !errors.Is(err, domain.ErrStoredObjectInUse) {
			return fmt.Errorf("未结算文件应保护内容对象：%v", err)
		}
		candidates, err := txStore.ListPurgeLeafCandidates(txContext, 1)
		if err != nil {
			return err
		}
		if len(candidates) != 1 || candidates[0].EntryID != 31 {
			return fmt.Errorf("叶子候选不符：%+v", candidates)
		}
		leaf, err := txStore.LockPurgeLeaf(txContext, candidates[0])
		if err != nil {
			return err
		}
		if err := txStore.markPurgeLeafCompleted(txContext, leaf); err != nil {
			return err
		}
		// 已完成上传仍有审计引用，当前内容引用检查应通过。
		return txStore.checkStoredObjectUnreferenced(txContext, object)
	})
	if err != nil {
		t.Fatal("清理前后的文件引用复核失败", err)
	}

	// 3. 先建立旧快照，再从另一连接写入历史夹具，复核必须读到新引用。
	err = store.WithPurgeRootTransaction(ctx, roots[0], func(txContext context.Context, txStore *Store) error {
		var historyRows uint64
		if err := txStore.database.WithContext(txContext).Raw("SELECT COUNT(*) FROM file_versions").Row().Scan(&historyRows); err != nil {
			return err
		}
		if historyRows != 0 {
			return fmt.Errorf("当前读夹具的旧快照应为空")
		}
		if _, err := pool.ExecContext(ctx, `INSERT INTO file_versions(user_id,entry_id,revision,object_id,file_size,created_at,retained_until)
			VALUES(7,31,1,29,16777216,UTC_TIMESTAMP(6)-INTERVAL 31 DAY,UTC_TIMESTAMP(6)-INTERVAL 1 DAY)`); err != nil {
			return err
		}
		object, err := lockObject(txContext, txStore)
		if err != nil {
			return err
		}
		return txStore.checkStoredObjectUnreferenced(txContext, object)
	})
	if !errors.Is(err, domain.ErrStoredObjectInUse) {
		t.Fatal("当前读未识别旧快照建立后已提交的历史引用", err)
	}
	if _, err := pool.ExecContext(ctx, "DELETE FROM file_versions WHERE user_id=7 AND entry_id=31 AND revision=1"); err != nil {
		t.Fatal(err)
	}

	// 4. 对未终结任务的发布引用继续验证同样的当前读语义。
	err = store.WithPurgeRootTransaction(ctx, roots[0], func(txContext context.Context, txStore *Store) error {
		task, err := txStore.FindUserUploadTask(txContext, 7, 19)
		if err != nil {
			return err
		}
		if task.Status != domain.UploadStatusCompleted || task.PublishedObjectID == nil || *task.PublishedObjectID != ready.ID {
			return fmt.Errorf("上传审计引用夹具不符：%+v", task)
		}
		if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET status='VERIFYING' WHERE user_id=7 AND id=19"); err != nil {
			return err
		}
		object, err := lockObject(txContext, txStore)
		if err != nil {
			return err
		}
		return txStore.checkStoredObjectUnreferenced(txContext, object)
	})
	if !errors.Is(err, domain.ErrStoredObjectInUse) {
		t.Fatal("当前读未识别新生效的发布引用", err)
	}
	after, err := store.FindUserStoredObject(ctx, 7, 29)
	if err != nil || !reflect.DeepEqual(after, ready) {
		t.Fatal("引用复核改动了对象状态或计时", after, err)
	}
	t.Log("旧快照不包含历史行，按账号和对象当前读仍识别新提交的到期历史引用")
}

func TestHistoryReferencesProtectStoredObjectGCIntegration(t *testing.T) {
	// 1. 由合成历史行独占 READY 对象，保留原任务的审计引用作为对照。
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	pool, _ := uploadTaskDatabase(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	const ownerID, objectID = 7, 29
	execute := func(statement string) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("准备历史引用夹具失败", err)
		}
	}
	readObject := func() *domain.StoredObject {
		t.Helper()
		object, err := store.FindUserStoredObject(ctx, ownerID, objectID)
		if err != nil {
			t.Fatal(err)
		}
		return object
	}
	execute("UPDATE file_entries SET status='PURGED',object_id=NULL,purged_at=UTC_TIMESTAMP(6),revision=3 WHERE user_id=7 AND id=31")
	execute(`INSERT INTO file_versions(user_id,entry_id,revision,object_id,file_size,created_at,retained_until)
		VALUES(7,31,2,29,16777216,UTC_TIMESTAMP(6)-INTERVAL 1 DAY,UTC_TIMESTAMP(6)+INTERVAL 29 DAY)`)

	// 2. 历史行存在时阻止新计时，也阻止领取已到期的旧候选。
	checkTimerBlocked := func() {
		t.Helper()
		before := readObject()
		err := store.withLockedUserTransaction(ctx, ownerID, true, func(txContext context.Context, txStore *Store) error {
			timed, err := txStore.StartReadyObjectGCTimer(txContext, objectID)
			if timed != nil {
				t.Fatal("历史保护期间交付了计时对象")
			}
			return err
		})
		if !errors.Is(err, domain.ErrStoredObjectInUse) || !reflect.DeepEqual(readObject(), before) {
			t.Fatal("历史引用未挡住计时，或对象被写入", err)
		}
	}
	checkTimerBlocked()
	execute(`UPDATE stored_objects SET unreferenced_at=UTC_TIMESTAMP(6)-INTERVAL 8 DAY,
		gc_after=UTC_TIMESTAMP(6)-INTERVAL 1 DAY WHERE owner_user_id=7 AND id=29`)
	candidates, err := store.ListStoredObjectGCCandidates(ctx, 1)
	if err != nil || len(candidates) != 1 || candidates[0].ObjectID != objectID {
		t.Fatal("未读到指定的回收候选夹具", candidates, err)
	}
	beforeClaim := readObject()
	checkClaimBlocked := func() {
		t.Helper()
		claimed, err := store.ClaimStoredObjectGC(ctx, candidates[0])
		if claimed != nil || !errors.Is(err, domain.ErrStoredObjectInUse) || !reflect.DeepEqual(readObject(), beforeClaim) {
			t.Fatal("仍有历史引用的对象被领取或修改", claimed, err)
		}
	}
	checkClaimBlocked()
	execute(`INSERT INTO file_versions(user_id,entry_id,revision,object_id,file_size,created_at,retained_until)
		VALUES(7,31,1,29,16777216,UTC_TIMESTAMP(6)-INTERVAL 31 DAY,UTC_TIMESTAMP(6)-INTERVAL 1 DAY)`)
	execute("DELETE FROM file_versions WHERE user_id=7 AND entry_id=31 AND revision=2")
	checkTimerBlocked()
	checkClaimBlocked()

	// 3. 移除最后一条合成历史行，复核原候选可领取。测试只检查数据库删除意图。
	execute("DELETE FROM file_versions WHERE user_id=7 AND entry_id=31 AND revision=1")
	claimed, err := store.ClaimStoredObjectGC(ctx, candidates[0])
	if err != nil || claimed == nil || claimed.Status != domain.StoredObjectStatusDeleting {
		t.Fatal("历史行移除后原候选未通过正常回收复核", claimed, err)
	}
	t.Log("在保留期内和仅剩已到期历史行时，计时和领取均被挡住。移除最后一条合成历史行后可取得删除意图")
}
