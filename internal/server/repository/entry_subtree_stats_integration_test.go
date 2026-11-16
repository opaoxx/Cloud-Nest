package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestFindUserEntrySubtreeStatsIntegration(t *testing.T) {
	// 1. 正式表独占库准备多层、空目录、文件和各生命周期保留节点。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(2)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash,status) VALUES(1,'subtree_one','子树统计用户','fixture_hash','ACTIVE'),(3,'subtree_other','其他用户','fixture_hash','ACTIVE')",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','subtree-object',1)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(33,3,NULL,'',X'','DIR'),(34,3,33,'其他账号子项',CAST('其他账号子项' AS BINARY),'DIR'),(12,1,11,'源目录',CAST('源目录' AS BINARY),'DIR'),(14,1,11,'旁支目录',CAST('旁支目录' AS BINARY),'DIR'),(15,1,12,'子目录',CAST('子目录' AS BINARY),'DIR'),(16,1,15,'孙目录',CAST('孙目录' AS BINARY),'DIR'),(17,1,12,'空目录',CAST('空目录' AS BINARY),'DIR')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(13,1,12,'文件',CAST('文件' AS BINARY),'FILE','OTHER',41,1),(21,1,16,'深层文件',CAST('深层文件' AS BINARY),'FILE','OTHER',41,1)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at,purge_started_at,purged_at) VALUES(18,1,12,'回收目录',CAST('回收目录' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6),NULL,NULL),(19,1,12,'清理目录',CAST('清理目录' AS BINARY),'DIR','PURGING',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6),NULL),(20,1,12,'墓碑目录',CAST('墓碑目录' AS BINARY),'DIR','PURGED',UTC_TIMESTAMP(6),NULL,UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,status,deleted_at,purged_at) VALUES(22,1,18,'回收目录中的文件',CAST('回收目录中的文件' AS BINARY),'FILE','OTHER',41,1,'ACTIVE',NULL,NULL),(23,1,19,'已清理文件',CAST('已清理文件' AS BINARY),'FILE','OTHER',NULL,1,'PURGED',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6))",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	check := func(t *testing.T, user, source, count, depth uint64) {
		t.Helper()
		result, err := store.FindUserEntrySubtreeStats(ctx, user, source)
		if err != nil || result == nil || result.UserID != user || result.EntryID != source || result.NodeCount != count || result.MaxRelativeDepth != depth {
			t.Fatal("实际子树统计错误", source, result, err)
		}
	}
	validateMove := func(source, target uint64) error {
		return store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, txStore *Store) error {
			checker := service.NewEntryMoveTargetService(func(ctx context.Context, action func(context.Context, service.FileEntryPathReader) error) error {
				return action(ctx, txStore)
			})
			paths, err := checker.Check(snapshotContext, domain.Actor{UserID: 1, DeviceID: 19, SessionID: 17}, source, target)
			if err != nil {
				return err
			}
			stats, err := txStore.FindUserEntrySubtreeStats(snapshotContext, 1, source)
			if err != nil {
				return err
			}
			return service.ValidateEntryMoveLimits(paths, stats)
		})
	}
	t.Run("file empty directory nested tree and roots", func(t *testing.T) {
		for _, item := range []struct{ source, count, depth uint64 }{{13, 1, 0}, {17, 1, 0}, {15, 3, 2}, {12, 11, 3}, {11, 13, 4}, {18, 2, 1}, {19, 2, 1}, {20, 1, 0}, {23, 1, 0}} {
			check(t, 1, item.source, item.count, item.depth)
		}
		check(t, 3, 33, 2, 1)
	})
	t.Run("missing and cross user scopes", func(t *testing.T) {
		for _, scope := range []struct{ user, source uint64 }{{1, 999}, {1, 33}, {3, 12}, {99, 12}} {
			if result, err := store.FindUserEntrySubtreeStats(ctx, scope.user, scope.source); result != nil || !errors.Is(err, domain.ErrFileEntryNotFound) {
				t.Fatal("范围缺失未返回不存在", err)
			}
		}
	})
	t.Run("relationship statistics and limits compose", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(2)
		for _, pair := range []struct{ source, target uint64 }{{12, 14}, {15, 11}, {13, 11}} {
			if err := validateMove(pair.source, pair.target); err != nil {
				t.Fatal("真实关系与子树统计限制组合错误", err)
			}
		}
	})
	t.Run("current snapshot and single connection", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(2)
		if err := store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, txStore *Store) error {
			result, err := txStore.FindUserEntrySubtreeStats(snapshotContext, 1, 12)
			if err != nil {
				return err
			}
			if result.NodeCount != 11 || result.MaxRelativeDepth != 3 {
				return errors.New("事务内统计错误")
			}
			return nil
		}); err != nil {
			t.Fatal("统计未复用单连接事务", err)
		}
	})
	t.Run("consistent snapshot across concurrent insertion", func(t *testing.T) {
		var beforeCount uint64
		if err := store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, txStore *Store) error {
			first, err := txStore.FindUserEntrySubtreeStats(snapshotContext, 1, 12)
			if err != nil {
				return err
			}
			beforeCount = first.NodeCount
			if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(64,1,12,'并发新增',CAST('并发新增' AS BINARY),'DIR')"); err != nil {
				return err
			}
			second, err := txStore.FindUserEntrySubtreeStats(snapshotContext, 1, 12)
			if err != nil {
				return err
			}
			if first.NodeCount != second.NodeCount || first.MaxRelativeDepth != second.MaxRelativeDepth {
				return errors.New("统计混入同一快照之后的新增")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		check(t, 1, 12, beforeCount+1, 3)
	})
	t.Run("cycles and file used as parent", func(t *testing.T) {
		for _, statement := range []string{
			"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(60,1,11,'循环源',CAST('循环源' AS BINARY),'DIR'),(61,1,60,'循环子',CAST('循环子' AS BINARY),'DIR'),(62,1,11,'自身循环',CAST('自身循环' AS BINARY),'DIR'),(63,1,13,'错误父项子目录',CAST('错误父项子目录' AS BINARY),'DIR')",
			"UPDATE file_entries SET parent_id=61 WHERE id=60",
			"UPDATE file_entries SET parent_id=62 WHERE id=62",
		} {
			if _, err := pool.ExecContext(ctx, statement); err != nil {
				t.Fatal(err)
			}
		}
		for _, source := range []uint64{60, 62, 13, 12} {
			if result, err := store.FindUserEntrySubtreeStats(ctx, 1, source); result != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
				t.Fatal("异常父链统计未拒绝", source, err)
			}
		}
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=11 WHERE id IN (60,62)"); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=14 WHERE id=63"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("relative depth limit and cancellation", func(t *testing.T) {
		parent := uint64(11)
		for depth := 0; depth <= domain.MaxFileEntryDepth; depth++ {
			id, name := uint64(100+depth), fmt.Sprintf("relative%d", depth)
			if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,1,?,?,?,'DIR')", id, parent, name, []byte(name)); err != nil {
				t.Fatal(err)
			}
			parent = id
		}
		check(t, 1, 100, 33, 32)
		// 移动源及后代在新位置的最终深度，同时核对文件和目录边界。
		if err := validateMove(101, 11); err != nil {
			t.Fatal("目录移动后恰好 32 层被拒绝", err)
		}
		if err := validateMove(100, 11); !errors.Is(err, domain.ErrFileEntryDepthExceeded) {
			t.Fatal("目录移动后 33 层未拒绝", err)
		}
		if err := validateMove(13, 130); err != nil {
			t.Fatal("文件移动后恰好 32 层被拒绝", err)
		}
		if err := validateMove(13, 131); !errors.Is(err, domain.ErrFileEntryDepthExceeded) {
			t.Fatal("文件移动后 33 层未拒绝", err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(133,1,?,'relative33',CAST('relative33' AS BINARY),'DIR')", parent); err != nil {
			t.Fatal(err)
		}
		if result, err := store.FindUserEntrySubtreeStats(ctx, 1, 100); result != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
			t.Fatal("超深统计未拒绝", err)
		}
		cancelled, cancelCall := context.WithCancel(ctx)
		cancelCall()
		if result, err := store.FindUserEntrySubtreeStats(cancelled, 1, 12); result != nil || !errors.Is(err, context.Canceled) {
			t.Fatal("取消未传播", err)
		}
		check(t, 1, 12, 12, 3)
	})
	t.Run("ten thousand and exact larger count", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(300,1,11,'大子树',CAST('大子树' AS BINARY),'DIR')"); err != nil {
			t.Fatal(err)
		}
		for start := 0; start < 9999; start += 500 {
			end := start + 500
			if end > 9999 {
				end = 9999
			}
			values := make([]string, 0, end-start)
			arguments := make([]any, 0, (end-start)*3)
			for index := start; index < end; index++ {
				id := 1000 + index
				name := fmt.Sprintf("node-%d", id)
				values = append(values, "(?,1,300,?,?,'DIR')")
				arguments = append(arguments, id, name, []byte(name))
			}
			if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES "+strings.Join(values, ","), arguments...); err != nil {
				t.Fatal(err)
			}
		}
		check(t, 1, 300, 10000, 1)
		if err := validateMove(300, 14); err != nil {
			t.Fatal("10000 项真实子树被拒绝", err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(20000,1,300,'extra-node',CAST('extra-node' AS BINARY),'DIR')"); err != nil {
			t.Fatal(err)
		}
		check(t, 1, 300, 10001, 1)
		if err := validateMove(300, 14); !errors.Is(err, domain.ErrFileEntrySubtreeTooLarge) {
			t.Fatal("10001 项真实子树未拒绝", err)
		}
	})
	t.Run("maximum unsigned owner and entry", func(t *testing.T) {
		maximum := ^uint64(0)
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,1,11,'高位源',CAST('高位源' AS BINARY),'DIR'),(?,1,?,'高位子',CAST('高位子' AS BINARY),'DIR')", maximum, maximum-1, maximum); err != nil {
			t.Fatal(err)
		}
		check(t, 1, maximum, 2, 1)
		if err := validateMove(maximum, 14); err != nil {
			t.Fatal("最大源标识的移动限制组合错误", err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash,status) VALUES(?,'maximum_owner','高位用户','fixture_hash','ACTIVE')", maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,?,NULL,'',X'','DIR')", maximum-2, maximum); err != nil {
			t.Fatal(err)
		}
		check(t, maximum, maximum-2, 1, 0)
	})
}
