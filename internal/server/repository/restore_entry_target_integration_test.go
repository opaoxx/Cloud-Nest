package repository

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestRestoreEntryTargetIntegration(t *testing.T) {
	// 1. 复用独占库和单连接设置，准备不同状态的恢复目标。
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	for _, statement := range []string{
		"UPDATE file_entries SET status='TRASHED', deleted_at='2000-01-01 00:00:00.000000' WHERE user_id=1 AND id IN (13,15,16)",
		`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at,purge_started_at,purged_at)
		  VALUES (17,1,11,'清理中',CAST('清理中' AS BINARY),'DIR','PURGING',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6),NULL),
		         (18,1,11,'已清理',CAST('已清理' AS BINARY),'DIR','PURGED',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6),UTC_TIMESTAMP(6))`,
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("准备恢复目标夹具失败", err)
		}
	}
	readEntries := func() map[uint64]domain.FileEntry {
		t.Helper()
		entries := make(map[uint64]domain.FileEntry)
		for _, entryID := range []uint64{11, 12, 13, 14, 15, 16, 17, 18} {
			entry, err := store.FindUserFileEntry(ctx, actor.UserID, entryID)
			if err != nil {
				t.Fatal("读取独占库目录项失败", err)
			}
			entries[entryID] = *entry
		}
		return entries
	}
	before := readEntries()

	// 2. 在同一已认证事务中检查目标记录和版本规则。
	for _, testCase := range []struct {
		name              string
		entryID, revision uint64
		want              error
	}{
		{"file still trashed", 13, 3, nil},
		{"directory", 14, 1, nil},
		{"nested candidate for later path check", 15, 5, nil},
		{"real root", 11, 1, domain.ErrRootEntryRestore},
		{"foreign target", 21, 1, domain.ErrFileEntryNotFound},
		{"missing target", 99, 1, domain.ErrFileEntryNotFound},
		{"active target", 12, 1, domain.ErrFileEntryNotFound},
		{"target being purged", 17, 1, domain.ErrFileEntryNotFound},
		{"purged target", 18, 1, domain.ErrFileEntryNotFound},
		{"stale revision", 13, 2, domain.ErrEntryRevisionConflict},
		{"exhausted revision", 16, math.MaxUint64, domain.ErrEntryRevisionExhausted},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var target *domain.FileEntry
			err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				current, err := service.ReadRestoreEntryTarget(txContext, actor, testCase.entryID, txStore)
				if err != nil {
					return err
				}
				if err := service.ValidateRestoreEntryRevision(current.Revision, testCase.revision); err != nil {
					return err
				}
				target = current
				return nil
			})
			if !errors.Is(err, testCase.want) {
				t.Fatalf("事务错误为 %v，期望 %v", err, testCase.want)
			}
			if testCase.want == nil {
				if target == nil || !reflect.DeepEqual(*target, before[testCase.entryID]) {
					t.Fatalf("目标与独占库记录不符，目标为 %+v", target)
				}
				return
			}
			if target != nil {
				t.Fatal("事务拒绝后仍然交付目标")
			}
			if testCase.want == domain.ErrEntryRevisionConflict {
				var conflict *domain.EntryRevisionConflictError
				if !errors.As(err, &conflict) || conflict.CurrentRevision != 3 {
					t.Fatalf("没有保留当前版本，错误为 %v", err)
				}
			}
		})
	}

	// 3. 比较测试前后的记录，确认本轮只读检查的结果。
	if !reflect.DeepEqual(readEntries(), before) {
		t.Fatal("恢复目标检查改变了数据库记录")
	}
}

func TestRestoreEntryAncestorsIntegration(t *testing.T) {
	// 1. 在独占库准备活动、回收和清理父链，所有恢复目标均保留原父目录。
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	for _, statement := range []string{
		"UPDATE file_entries SET status='TRASHED', deleted_at='2000-01-01 00:00:00.000000' WHERE user_id=1 AND id IN (13,15)",
		`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at,purge_started_at,purged_at)
		  VALUES(17,1,11,'purging',CAST('purging' AS BINARY),'DIR','PURGING',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6),NULL),
		        (18,1,11,'purged',CAST('purged' AS BINARY),'DIR','PURGED',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6),UTC_TIMESTAMP(6))`,
		`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at)
		  VALUES(22,1,17,'middle',CAST('middle' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6)),
		        (23,1,18,'middle',CAST('middle' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6))`,
		`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at,revision)
		  VALUES(24,1,22,'target',CAST('target' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6),2),
		        (25,1,23,'target',CAST('target' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6),2)`,
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("建立恢复父链夹具失败", err)
		}
	}
	readEntries := func() map[uint64]*domain.FileEntry {
		t.Helper()
		entries := make(map[uint64]*domain.FileEntry)
		for _, entryID := range []uint64{11, 12, 13, 14, 15, 17, 18, 22, 23, 24, 25} {
			entry, err := store.FindUserFileEntry(ctx, actor.UserID, entryID)
			if err != nil {
				t.Fatal("读取恢复父链夹具失败", err)
			}
			entries[entryID] = entry
		}
		return entries
	}
	before := readEntries()

	// 2. 在同一个用户事务先读目标，再查父链，最后验证版本。
	for _, scenario := range []struct {
		name             string
		entryID          uint64
		expectedRevision uint64
		want             *service.RestoreEntryAncestors
		wantError        error
	}{
		{name: "original parent is root", entryID: 14, expectedRevision: 1, want: &service.RestoreEntryAncestors{RootEntryID: 11}},
		{name: "active path", entryID: 13, expectedRevision: 3, want: &service.RestoreEntryAncestors{RootEntryID: 11, OriginalParentDepth: 1}},
		{name: "trashed parent", entryID: 15, expectedRevision: 5, want: &service.RestoreEntryAncestors{RootEntryID: 11, OriginalParentDepth: 1, HasTrashedAncestor: true}},
		{name: "purging grandparent before revision conflict", entryID: 24, expectedRevision: 1, wantError: domain.ErrFileEntryNotFound},
		{name: "purged grandparent before revision conflict", entryID: 25, expectedRevision: 1, wantError: domain.ErrFileEntryNotFound},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var chain *service.RestoreEntryAncestors
			err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				target, err := service.ReadRestoreEntryTarget(txContext, actor, scenario.entryID, txStore)
				if err != nil {
					return err
				}
				chain, err = service.ReadRestoreEntryAncestors(txContext, actor, target, txStore)
				if err != nil {
					return err
				}
				return service.ValidateRestoreEntryRevision(target.Revision, scenario.expectedRevision)
			})
			if !errors.Is(err, scenario.wantError) || !reflect.DeepEqual(chain, scenario.want) {
				t.Fatalf("事务父链为 %+v，期望 %+v，错误为 %v，期望 %v", chain, scenario.want, err, scenario.wantError)
			}
		})
	}

	// 3. 对照前后记录，确认父链检查只读取目录事实。
	if !reflect.DeepEqual(readEntries(), before) {
		t.Fatal("恢复父链检查改变了数据库记录")
	}
}
