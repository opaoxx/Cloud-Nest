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

	// 3. 先读出旧快照，再提交任务状态变化。复核必须读到新生效的发布引用。
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
}
