package repository

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestResolveRestoreEntryNameIntegration(t *testing.T) {
	// 1. 准备回收目标以及活动、回收、跨目录和跨用户的同名项。
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	for _, target := range []struct {
		id   uint64
		name string
	}{{13, "Cafe\u0301.TXT"}, {14, "资料.V2"}, {15, "隐藏.md"}} {
		key, err := service.BuildEntryNameKey(target.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, `UPDATE file_entries SET name=?, name_key=?, status='TRASHED', deleted_at='2000-01-01 00:00:00.000000'
			WHERE user_id=? AND id=?`, target.name, key, actor.UserID, target.id); err != nil {
			t.Fatal("准备回收目标失败", err)
		}
	}
	for _, entry := range []struct {
		userID, parentID uint64
		name, status     string
	}{
		{1, 12, "Café.txt", "ACTIVE"},
		{1, 12, "CAFÉ (1).txt", "ACTIVE"},
		{1, 12, "CAFÉ (2).txt", "TRASHED"},
		{1, 12, "CAFÉ (3).txt", "ACTIVE"},
		{1, 11, "CAFÉ (2).txt", "ACTIVE"},
		{2, 21, "CAFÉ (2).txt", "ACTIVE"},
		{1, 11, "资料.v2", "ACTIVE"},
		{1, 11, "隐藏.md", "ACTIVE"},
		{1, 14, "隐藏 (1).md", "ACTIVE"},
	} {
		key, err := service.BuildEntryNameKey(entry.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, `INSERT INTO file_entries(user_id,parent_id,name,name_key,entry_type,status,deleted_at)
			VALUES(?,?,?,?,'DIR',?,CASE WHEN ?='TRASHED' THEN UTC_TIMESTAMP(6) END)`,
			entry.userID, entry.parentID, entry.name, key, entry.status, entry.status); err != nil {
			t.Fatal("准备同名项失败", err)
		}
	}

	// 2. 在同一已认证用户事务中按实际父目录查重。
	for _, scenario := range []struct {
		name     string
		targetID uint64
		parentID uint64
		chosen   string
	}{
		{"normalized name and smallest gap", 13, 12, "Cafe\u0301 (2).TXT"},
		{"directory with dot", 14, 11, "资料.V2 (1)"},
		{"fallback checks actual root", 15, 11, "隐藏 (1).md"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			before, err := store.FindUserFileEntry(ctx, actor.UserID, scenario.targetID)
			if err != nil {
				t.Fatal(err)
			}
			var parentID uint64
			var chosen string
			err = store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				target, err := service.ReadRestoreEntryTarget(txContext, actor, scenario.targetID, txStore)
				if err != nil {
					return err
				}
				ancestors, err := service.ReadRestoreEntryAncestors(txContext, actor, target, txStore)
				if err != nil {
					return err
				}
				if err := service.ValidateRestoreEntryRevision(target.Revision, before.Revision); err != nil {
					return err
				}
				stats, err := txStore.FindUserEntrySubtreeStats(txContext, actor.UserID, target.ID)
				if err != nil {
					return err
				}
				parentID, err = service.SelectRestoreEntryParent(target, ancestors, stats)
				if err != nil {
					return err
				}
				chosen, err = service.ResolveRestoreEntryName(txContext, target, parentID, txStore.FindUserActiveFileEntryByName)
				return err
			})
			if err != nil || parentID != scenario.parentID || chosen != scenario.chosen {
				t.Fatalf("得到父目录 %d、名称 %q 和错误 %v，期望 %d 和 %q", parentID, chosen, err, scenario.parentID, scenario.chosen)
			}

			// 3. 验证选名只读取目录事实。
			after, err := store.FindUserFileEntry(ctx, actor.UserID, scenario.targetID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("选名前后的目标不一致，错误为 %v", err)
			}
		})
	}
}
