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

func TestValidateFileReuseTargetIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	addDirectory := func(id, parentID uint64) {
		t.Helper()
		name := fmt.Sprintf("fixture-dir-%d", id)
		if _, err := pool.ExecContext(ctx,
			"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,?,?,?,?,'DIR')",
			id, actor.UserID, parentID, name, []byte(name)); err != nil {
			t.Fatal(err)
		}
	}

	// 1. 建立两个活动子目录和一条达到深度上限的活动路径。
	const rootID, activeParentID, trashedParentID = 11, 12, 14
	const hiddenChildID, visibleChildID = 22, 23
	addDirectory(hiddenChildID, trashedParentID)
	addDirectory(visibleChildID, activeParentID)
	parentID := uint64(rootID)
	for depth := 1; depth <= domain.MaxFileEntryDepth; depth++ {
		id := uint64(100 + depth)
		addDirectory(id, parentID)
		parentID = id
	}
	before, err := store.FindUserFileEntry(ctx, actor.UserID, visibleChildID)
	if err != nil {
		t.Fatal(err)
	}
	check := func(parentID uint64, wantPathLength int, wantError error) {
		t.Helper()
		var path *domain.FileEntryPath
		err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			var checkError error
			path, checkError = service.ValidateFileReuseTarget(txContext, actor, parentID, txStore)
			return checkError
		})
		if !errors.Is(err, wantError) {
			t.Fatal("账号事务内的目标校验结果不符", parentID, err)
		}
		if wantError != nil {
			if path != nil {
				t.Fatal("拒绝后仍交付目标路径", path)
			}
			return
		}
		if path == nil || len(path.Entries) != wantPathLength ||
			path.Entries[0].ID != rootID || path.Entries[len(path.Entries)-1].ID != parentID {
			t.Fatal("未交付完整的同账号目标路径", parentID, path)
		}
	}

	// 2. 验证根目录、可见目录、跨账号、隐藏路径和新文件深度边界。
	check(rootID, 1, nil)
	check(visibleChildID, 3, nil)
	check(21, 0, domain.ErrFileEntryNotFound)
	check(99, 0, domain.ErrFileEntryNotFound)
	check(13, 0, domain.ErrFileEntryNotDirectory)
	check(trashedParentID, 0, domain.ErrFileEntryNotFound)
	check(hiddenChildID, 0, domain.ErrFileEntryNotFound)
	check(parentID-1, domain.MaxFileEntryDepth, nil)
	check(parentID, 0, domain.ErrFileEntryDepthExceeded)
	after, err := store.FindUserFileEntry(ctx, actor.UserID, visibleChildID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("目标校验改变了目录记录", after, err)
	}

	// 3. 将已验证目标的上级目录正式移入回收站，再复核原目标。
	if err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
		_, err := txStore.TrashUserFileEntry(txContext, activeParentID, 1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	child, err := store.FindUserFileEntry(ctx, actor.UserID, visibleChildID)
	if err != nil || child.Status != "ACTIVE" {
		t.Fatal("目标夹具未保持活动状态", child, err)
	}
	check(visibleChildID, 0, domain.ErrFileEntryNotFound)
	t.Log("账号事务确认完整目标路径、32 层边界和上级回收后的失效")
}
