package repository

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestReleasePurgeLeafQuotaIntegration(t *testing.T) {
	// 1. 准备清理中的根项、普通文件、零字节文件和空目录。
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	pool, store, _ := trashEntryTestDatabase(t, ctx)
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal("准备清理配额夹具失败", err)
		}
	}
	stamp := time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC)
	execute("UPDATE file_entries SET status='PURGING',purge_started_at=?,revision=revision+1 WHERE id=14", stamp)
	execute(`INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size)
		VALUES (42,1,'MINIO','fixture-bucket','purge-empty-file',0)`)
	execute(`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type)
		VALUES (17,1,14,'待清理空目录',CAST('待清理空目录' AS BINARY),'DIR')`)
	execute(`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size)
		VALUES (18,1,14,'空文件.md',CAST('空文件.md' AS BINARY),'FILE','DOCUMENT',42,0)`)
	roots, err := store.ListPurgeRootCandidates(ctx, 1)
	if err != nil || len(roots) != 1 || roots[0].EntryID != 14 {
		t.Fatal("未取得清理根项", roots, err)
	}
	root := roots[0]
	file := domain.PurgeLeafCandidate{EntryID: 15, ParentID: 14, EntryType: "FILE", Status: "ACTIVE", Revision: 5, FileSize: 2048}
	directory := domain.PurgeLeafCandidate{EntryID: 17, ParentID: 14, EntryType: "DIR", Status: "ACTIVE", Revision: 1}
	emptyFile := domain.PurgeLeafCandidate{EntryID: 18, ParentID: 14, EntryType: "FILE", Status: "ACTIVE", Revision: 1}
	rollbackFixture := errors.New("验证配额与状态写入后回滚")
	const quotaBytes = 10000
	const reservedBytes = 512

	// 2. 复用根项事务比较扣减、余额不足及无需扣减的分支。
	for _, scenario := range []struct {
		name, accountStatus string
		candidate           domain.PurgeLeafCandidate
		used, wantUsed      uint64
		wantFailure         error
	}{
		{"normal file", "ACTIVE", file, 4096, 2048, nil},
		{"exact balance", "ACTIVE", file, 2048, 0, nil},
		{"insufficient balance", "ACTIVE", file, 2047, 2047, domain.ErrInvalidAccountData},
		{"disabled account", "DISABLED", file, 4096, 2048, nil},
		{"directory", "ACTIVE", directory, 4096, 4096, nil},
		{"empty file", "ACTIVE", emptyFile, 4096, 4096, nil},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			execute("UPDATE users SET status=?,storage_quota=?,used_storage=?,reserved_storage=?,updated_at=? WHERE id=1",
				scenario.accountStatus, quotaBytes, scenario.used, reservedBytes, stamp)
			err := store.WithPurgeRootTransaction(ctx, root, func(txContext context.Context, txStore *Store) error {
				leaf, err := txStore.LockPurgeLeaf(txContext, scenario.candidate)
				if err != nil {
					return err
				}
				if err := txStore.releasePurgeLeafQuota(txContext, leaf); err != nil {
					return err
				}

				var quota, used, reserved uint64
				var updatedAt time.Time
				err = txStore.database.WithContext(txContext).Raw(`
					SELECT storage_quota,used_storage,reserved_storage,updated_at FROM users WHERE id=1`).
					Row().Scan(&quota, &used, &reserved, &updatedAt)
				if err != nil {
					return err
				}
				if quota != quotaBytes || used != scenario.wantUsed || reserved != reservedBytes ||
					updatedAt.Equal(stamp) != (scenario.used == scenario.wantUsed) {
					return fmt.Errorf("扣减后的账户账目不符：配额 %d，已用 %d，预占 %d，更新时间 %v", quota, used, reserved, updatedAt)
				}
				if err := txStore.markPurgeLeafCompleted(txContext, leaf); err != nil {
					return err
				}
				return rollbackFixture
			})
			wantError := scenario.wantFailure
			if wantError == nil {
				wantError = rollbackFixture
			}
			if !errors.Is(err, wantError) {
				t.Fatal("扣减错误或回滚原因不符", err)
			}

			// 3. 回查事务之外的账户与目录项，确认失败没有留下单边写入。
			var used, reserved uint64
			var updatedAt time.Time
			if err := pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage,updated_at FROM users WHERE id=1").
				Scan(&used, &reserved, &updatedAt); err != nil || used != scenario.used || reserved != reservedBytes || !updatedAt.Equal(stamp) {
				t.Fatal("配额写入未回滚", used, reserved, updatedAt, err)
			}
			entry, err := store.FindUserFileEntry(ctx, 1, scenario.candidate.EntryID)
			if err != nil || entry == nil || entry.Status != scenario.candidate.Status || entry.Revision != scenario.candidate.Revision {
				t.Fatal("目录项状态写入未回滚", entry, err)
			}
		})
	}
}
