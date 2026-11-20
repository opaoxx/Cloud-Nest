package repository

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestSelectRestoreEntryParentIntegration(t *testing.T) {
	// 1. 准备独占库，以及原位置和真实根目录的深度边界。
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='TRASHED', deleted_at=UTC_TIMESTAMP(6) WHERE user_id=1 AND id IN (13,15)"); err != nil {
		t.Fatal("准备回收文件失败", err)
	}
	for _, branch := range []struct {
		sourceID, parentID, relativeDepth uint64
	}{
		{100, 11, 31}, // 根目录内恰好 32 层。
		{200, 12, 31}, // 原父目录下为 33 层，回退到根目录后为 32 层。
		{300, 11, 32}, // 根目录下为 33 层，用于验证历史超深数据的拒绝分支。
	} {
		parentID := branch.parentID
		for depth := uint64(0); depth <= branch.relativeDepth; depth++ {
			entryID := branch.sourceID + depth
			status := "ACTIVE"
			var deletedAt any
			// 最深后代也处于回收状态，仍需计入恢复后的层级。
			if depth == 0 || depth == branch.relativeDepth {
				status, deletedAt = "TRASHED", time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
			}
			if _, err := pool.ExecContext(ctx, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at)
				VALUES(?,1,?,'深度边界目录',CAST('深度边界目录' AS BINARY),'DIR',?,?)`, entryID, parentID, status, deletedAt); err != nil {
				t.Fatal("准备恢复子树失败", entryID, err)
			}
			parentID = entryID
		}
	}

	// 2. 在单连接用户事务中读取目标、父链及子树统计，执行实际选址。
	for _, scenario := range []struct {
		name          string
		entryID       uint64
		relativeDepth uint64
		wantParent    uint64
		wantError     error
	}{
		{name: "original active parent", entryID: 13, wantParent: 12},
		{name: "fallback for trashed ancestor", entryID: 15, wantParent: 11},
		{name: "root fits exactly", entryID: 100, relativeDepth: 31, wantParent: 11},
		{name: "fallback includes deepest trashed descendant", entryID: 200, relativeDepth: 31, wantParent: 11},
		{name: "root too deep", entryID: 300, relativeDepth: 32, wantError: domain.ErrRestoreTargetUnavailable},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			before, err := store.FindUserFileEntry(ctx, actor.UserID, scenario.entryID)
			if err != nil {
				t.Fatal("读取选址夹具失败", err)
			}
			var parentID uint64
			err = store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				target, err := service.ReadRestoreEntryTarget(txContext, actor, scenario.entryID, txStore)
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
				if stats.MaxRelativeDepth != scenario.relativeDepth {
					return fmt.Errorf("真实统计深度为 %d，期望 %d", stats.MaxRelativeDepth, scenario.relativeDepth)
				}
				parentID, err = service.SelectRestoreEntryParent(target, ancestors, stats)
				return err
			})
			if !errors.Is(err, scenario.wantError) || parentID != scenario.wantParent {
				t.Fatalf("实际父目录为 %d，错误为 %v，期望 %d, %v", parentID, err, scenario.wantParent, scenario.wantError)
			}

			// 3. 复查选址前后目标位置、状态和版本等目录事实。
			after, err := store.FindUserFileEntry(ctx, actor.UserID, scenario.entryID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("选址前后的目录项不一致，错误为 %v", err)
			}
		})
	}
}
