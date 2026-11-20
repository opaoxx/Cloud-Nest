package repository

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestListUserPurgePreviewSubtreeIntegration(t *testing.T) {
	// 1. 在独占库准备回收目标、各状态后代、旁支和另一用户。
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	for _, statement := range []string{
		"UPDATE file_entries SET status='TRASHED', deleted_at=UTC_TIMESTAMP(6) WHERE user_id=1 AND id=13",
		`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at,purge_started_at)
		  VALUES (9,1,14,'回收后代',CAST('回收后代' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6),NULL),
		         (17,1,14,'清理中后代',CAST('清理中后代' AS BINARY),'DIR','PURGING',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6))`,
		`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,file_size,status,deleted_at,purged_at,revision)
		  VALUES (8,1,9,'保留墓碑.md',CAST('保留墓碑.md' AS BINARY),'FILE','DOCUMENT',1024,'PURGED',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6),7)`,
		`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,status,deleted_at)
		  VALUES (10,1,9,'共享对象.md',CAST('共享对象.md' AS BINARY),'FILE','DOCUMENT',41,2048,'TRASHED',UTC_TIMESTAMP(6))`,
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("准备清理子树夹具失败", err)
		}
	}

	// 2. 用单连接验证资格检查和子树读取共用一笔快照。
	t.Run("all states ordered by ID", func(t *testing.T) {
		want := []domain.PurgePreviewSubtreeEntry{
			{ID: 8, EntryType: "FILE", Status: "PURGED", Revision: 7, FileSize: 1024},
			{ID: 9, EntryType: "DIR", Status: "TRASHED", Revision: 1},
			{ID: 10, EntryType: "FILE", Status: "TRASHED", Revision: 1, FileSize: 2048},
			{ID: 14, EntryType: "DIR", Status: "TRASHED", Revision: 1},
			{ID: 15, EntryType: "FILE", Status: "ACTIVE", Revision: 5, FileSize: 2048},
			{ID: 17, EntryType: "DIR", Status: "PURGING", Revision: 1},
		}
		if err := store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, txStore *Store) error {
			target, err := service.ValidatePurgePreviewTarget(snapshotContext, actor, 14, txStore)
			if err != nil {
				return err
			}
			actual, err := txStore.ListUserPurgePreviewSubtree(snapshotContext, target.UserID, target.ID)
			if err != nil || !reflect.DeepEqual(actual, want) {
				return fmt.Errorf("子树记录为 %+v，错误 %v", actual, err)
			}
			// 两个文件引用对象 41，各计一次逻辑大小。已结算墓碑保留在范围中。
			summary, err := service.SummarizePurgePreviewSubtree(actual)
			if err != nil || summary != (domain.PurgePreviewSummary{FileCount: 2, DirectoryCount: 3, LogicalBytes: 4096}) {
				return fmt.Errorf("子树汇总为 %+v，错误 %v", summary, err)
			}
			fingerprint, err := service.BuildPurgePreviewSubtreeFingerprint(actual)
			if err != nil || fmt.Sprintf("%x", fingerprint) != "bc153a0f7df0fe9c078dab05127989f8c445b325f611c4989b3a506c2e5e921e" {
				return fmt.Errorf("实际子树指纹为 %x，错误 %v", fingerprint, err)
			}
			signer, err := service.NewPurgePreviewSigner(bytes.Repeat([]byte{9}, sha256.Size))
			if err != nil {
				return err
			}
			issuedAt := time.Date(2026, 11, 20, 4, 30, 0, 123456000, time.UTC)
			credential, err := signer.Sign(target.UserID, target.ID, fingerprint, issuedAt)
			if err != nil || credential.PreviewDigest == "" || !credential.ExpiresAt.Equal(issuedAt.Add(5*time.Minute)) {
				return fmt.Errorf("为实际清理范围签发预检凭证失败: %v", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("file target and isolation", func(t *testing.T) {
		entries, err := store.ListUserPurgePreviewSubtree(ctx, actor.UserID, 13)
		want := []domain.PurgePreviewSubtreeEntry{{ID: 13, EntryType: "FILE", Status: "TRASHED", Revision: 3, FileSize: 2048}}
		if err != nil || !reflect.DeepEqual(entries, want) {
			t.Fatal("文件目标应只返回自身", entries, err)
		}
		for _, entryID := range []uint64{21, 999} {
			if entries, err := store.ListUserPurgePreviewSubtree(ctx, actor.UserID, entryID); entries != nil || !errors.Is(err, domain.ErrFileEntryNotFound) {
				t.Fatal("跨用户或缺失目标未被拒绝", entries, err)
			}
		}
		cancelled, stop := context.WithCancel(ctx)
		stop()
		if entries, err := store.ListUserPurgePreviewSubtree(cancelled, actor.UserID, 14); entries != nil || !errors.Is(err, context.Canceled) {
			t.Fatal("取消未传播", entries, err)
		}
	})

	t.Run("consistent snapshot across concurrent insert", func(t *testing.T) {
		pool.SetMaxOpenConns(2)
		defer pool.SetMaxOpenConns(1)
		var originalFingerprint [sha256.Size]byte
		if err := store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, txStore *Store) error {
			target, err := service.ValidatePurgePreviewTarget(snapshotContext, actor, 14, txStore)
			if err != nil {
				return err
			}
			beforeInsert, err := txStore.ListUserPurgePreviewSubtree(snapshotContext, target.UserID, target.ID)
			if err != nil {
				return err
			}
			originalFingerprint, err = service.BuildPurgePreviewSubtreeFingerprint(beforeInsert)
			if err != nil {
				return err
			}
			if _, err := pool.ExecContext(snapshotContext, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(19,1,14,'并发加入',CAST('并发加入' AS BINARY),'DIR')"); err != nil {
				return err
			}
			entries, err := txStore.ListUserPurgePreviewSubtree(snapshotContext, target.UserID, target.ID)
			if err != nil || len(entries) != 6 {
				return fmt.Errorf("同一快照中子树结果为 %+v，错误 %v", entries, err)
			}
			current, err := service.BuildPurgePreviewSubtreeFingerprint(entries)
			if err != nil || current != originalFingerprint {
				return fmt.Errorf("同一快照内的指纹改变为 %x，错误 %v", current, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		entries, err := store.ListUserPurgePreviewSubtree(ctx, actor.UserID, 14)
		if err != nil || len(entries) != 7 || entries[len(entries)-1].ID != 19 {
			t.Fatal("新快照没有读到并发新增", entries, err)
		}
		fingerprint, err := service.BuildPurgePreviewSubtreeFingerprint(entries)
		if err != nil || fingerprint == originalFingerprint {
			t.Fatal("新快照指纹未反映并发新增", err)
		}
	})

	// 3. 验证真实 10000 条范围及额外墓碑触发的超限。
	t.Run("ten thousand and one more retained tombstone", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at) VALUES(60,1,11,'宽子树',CAST('宽子树' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6))"); err != nil {
			t.Fatal(err)
		}
		childCount := domain.MaxFileEntrySubtreeNodes - 1
		for start := 0; start < childCount; start += 500 {
			end := min(start+500, childCount)
			parameters := make([]any, 0, (end-start)*3)
			values := make([]string, 0, end-start)
			for index := start; index < end; index++ {
				entryID := 1000 + index
				name := fmt.Sprintf("node-%d", entryID)
				values = append(values, "(?,1,60,?,?,'DIR')")
				parameters = append(parameters, entryID, name, []byte(name))
			}
			if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES "+strings.Join(values, ","), parameters...); err != nil {
				t.Fatal(err)
			}
		}
		entries, err := store.ListUserPurgePreviewSubtree(ctx, actor.UserID, 60)
		if err != nil || len(entries) != domain.MaxFileEntrySubtreeNodes || entries[0].ID != 60 ||
			entries[len(entries)-1].ID != uint64(999+childCount) ||
			!slices.IsSortedFunc(entries, func(first, second domain.PurgePreviewSubtreeEntry) int {
				return cmp.Compare(first.ID, second.ID)
			}) {
			t.Fatal("10000 条范围或顺序不正确", len(entries), err)
		}
		if _, err := service.BuildPurgePreviewSubtreeFingerprint(entries); err != nil {
			t.Fatal("无法为 10000 条保留记录生成指纹", err)
		}
		if _, err := pool.ExecContext(ctx, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at,purged_at)
		  VALUES(20000,1,60,'额外墓碑',CAST('额外墓碑' AS BINARY),'DIR','PURGED',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6))`); err != nil {
			t.Fatal(err)
		}
		if entries, err := store.ListUserPurgePreviewSubtree(ctx, actor.UserID, 60); entries != nil || !errors.Is(err, domain.ErrFileEntrySubtreeTooLarge) {
			t.Fatal("10001 条范围应整体拒绝", len(entries), err)
		}
	})

	// 4. 拒绝循环、文件充当父目录和超过相对深度范围的结果。
	t.Run("invalid subtree structure", func(t *testing.T) {
		for _, statement := range []string{
			"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(70,1,11,'循环起点',CAST('循环起点' AS BINARY),'DIR'),(71,1,70,'循环后代',CAST('循环后代' AS BINARY),'DIR'),(72,1,13,'挂在文件下',CAST('挂在文件下' AS BINARY),'DIR')",
			"UPDATE file_entries SET parent_id=71 WHERE user_id=1 AND id=70",
		} {
			if _, err := pool.ExecContext(ctx, statement); err != nil {
				t.Fatal(err)
			}
		}
		parentID := uint64(11)
		for depth := 0; depth <= domain.MaxFileEntryDepth+1; depth++ {
			entryID := 80 + uint64(depth)
			if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,1,?,'深层',CAST('深层' AS BINARY),'DIR')", entryID, parentID); err != nil {
				t.Fatal(err)
			}
			parentID = entryID
		}
		for _, entryID := range []uint64{70, 13, 80} {
			if entries, err := store.ListUserPurgePreviewSubtree(ctx, actor.UserID, entryID); entries != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
				t.Fatal("异常子树未被拒绝", entryID, entries, err)
			}
		}
	})

	t.Run("maximum unsigned fields", func(t *testing.T) {
		maximum := uint64(math.MaxUint64)
		if _, err := pool.ExecContext(ctx, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,revision)
		  VALUES(?,1,11,'最大字段.md',CAST('最大字段.md' AS BINARY),'FILE','DOCUMENT',41,?,?)`, maximum, maximum, maximum); err != nil {
			t.Fatal(err)
		}
		entries, err := store.ListUserPurgePreviewSubtree(ctx, actor.UserID, maximum)
		want := []domain.PurgePreviewSubtreeEntry{{ID: maximum, EntryType: "FILE", Status: "ACTIVE", Revision: maximum, FileSize: maximum}}
		if err != nil || !reflect.DeepEqual(entries, want) {
			t.Fatal("无符号字段未完整保留", entries, err)
		}
		summary, err := service.SummarizePurgePreviewSubtree(entries)
		if err != nil || summary != (domain.PurgePreviewSummary{FileCount: 1, LogicalBytes: maximum}) {
			t.Fatal("最大逻辑用量的汇总不符", summary, err)
		}
	})
}
