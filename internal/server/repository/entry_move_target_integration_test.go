package repository

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestEntryMoveTargetServiceIntegration(t *testing.T) {
	// 1. 正式表独占库准备源、目的地、子孙目录和其他账号。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(2)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash,status) VALUES(1,'move_one','移动关系用户','fixture_hash','ACTIVE'),(2,'move_disabled','禁用用户','fixture_hash','DISABLED'),(3,'move_other','其他用户','fixture_hash','ACTIVE')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(22,2,NULL,'',X'','DIR'),(33,3,NULL,'',X'','DIR'),(12,1,11,'源目录',CAST('源目录' AS BINARY),'DIR'),(14,1,11,'其他目录',CAST('其他目录' AS BINARY),'DIR'),(15,1,12,'子目录',CAST('子目录' AS BINARY),'DIR'),(16,1,15,'孙目录',CAST('孙目录' AS BINARY),'DIR')",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','move-path-object',2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(13,1,12,'学习笔记.md',CAST('学习笔记.md' AS BINARY),'FILE','DOCUMENT',41,2048)",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Actor{UserID: 1, DeviceID: 19, SessionID: 17}
	withSnapshot := func(ctx context.Context, action func(context.Context, service.FileEntryPathReader) error) error {
		return store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, txStore *Store) error { return action(snapshotContext, txStore) })
	}
	checker := service.NewEntryMoveTargetService(withSnapshot)
	checkFailure := func(t *testing.T, source, target uint64, want error) {
		t.Helper()
		if result, err := checker.Check(ctx, actor, source, target); result != nil || !errors.Is(err, want) {
			t.Fatal("移动关系错误未按预期返回", source, target, err)
		}
	}
	// 2. 单连接完成两条完整路径读取，结果可供后续移动流程使用。
	t.Run("single connection valid relationships", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(2)
		for _, pair := range []struct{ source, target uint64 }{{13, 14}, {13, 11}, {13, 12}, {12, 14}, {12, 11}, {13, 15}, {15, 11}} {
			result, err := checker.Check(ctx, actor, pair.source, pair.target)
			if err != nil || result == nil || result.Source.Entries[len(result.Source.Entries)-1].ID != pair.source || result.TargetParent.Entries[len(result.TargetParent.Entries)-1].ID != pair.target {
				t.Fatal("合法关系未返回对应路径", err)
			}
		}
		result, err := checker.Check(ctx, actor, 13, 14)
		if err != nil {
			t.Fatal(err)
		}
		*result.Source.Entries[len(result.Source.Entries)-1].ParentID = 999
		actual, err := store.FindUserFileEntry(ctx, 1, 13)
		if err != nil || actual.ParentID == nil || *actual.ParentID != 12 || actual.Name != "学习笔记.md" {
			t.Fatal("结果修改影响了数据库事实", err)
		}
	})
	t.Run("root self subtree type and ownership", func(t *testing.T) {
		for _, testCase := range []struct {
			source, target uint64
			want           error
		}{
			{11, 14, domain.ErrRootEntryMove}, {12, 12, domain.ErrEntryMoveIntoSubtree}, {12, 15, domain.ErrEntryMoveIntoSubtree}, {12, 16, domain.ErrEntryMoveIntoSubtree},
			{13, 13, domain.ErrFileEntryNotDirectory}, {14, 13, domain.ErrFileEntryNotDirectory}, {999, 14, domain.ErrFileEntryNotFound}, {13, 999, domain.ErrFileEntryNotFound},
			{33, 14, domain.ErrFileEntryNotFound}, {13, 33, domain.ErrFileEntryNotFound},
		} {
			checkFailure(t, testCase.source, testCase.target, testCase.want)
		}
	})
	t.Run("all hidden source and destination chains", func(t *testing.T) {
		for _, testCase := range []struct{ hidden, source, target uint64 }{{13, 13, 14}, {12, 13, 14}, {14, 13, 14}, {12, 14, 15}} {
			for _, state := range []string{"TRASHED", "PURGING", "PURGED"} {
				if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status=?,deleted_at=UTC_TIMESTAMP(6),purge_started_at=IF(?='PURGING',UTC_TIMESTAMP(6),NULL),purged_at=IF(?='PURGED',UTC_TIMESTAMP(6),NULL),object_id=IF(entry_type='FILE' AND ?<>'PURGED',41,NULL) WHERE id=?", state, state, state, state, testCase.hidden); err != nil {
					t.Fatal(err)
				}
				result, operationError := checker.Check(ctx, actor, testCase.source, testCase.target)
				if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='ACTIVE',deleted_at=NULL,purge_started_at=NULL,purged_at=NULL,object_id=IF(entry_type='FILE',41,NULL) WHERE id=?", testCase.hidden); err != nil {
					t.Fatal(err)
				}
				if result != nil || !errors.Is(operationError, domain.ErrFileEntryNotFound) {
					t.Fatal("隐藏链未拒绝", state, operationError)
				}
			}
		}
	})
	t.Run("disabled profile", func(t *testing.T) {
		disabled := actor
		disabled.UserID = 2
		if result, err := checker.Check(ctx, disabled, 22, 22); result != nil || !errors.Is(err, domain.ErrAccountDisabled) {
			t.Fatal("禁用账号未拒绝", err)
		}
	})

	// 3. 受控并发变更验证两条路径共享同一一致性读取范围。
	t.Run("consistent snapshot across destination change", func(t *testing.T) {
		defer func() {
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=11,revision=revision+1 WHERE id=14"); err != nil {
				t.Error(err)
			}
		}()
		changed := false
		concurrent := service.NewEntryMoveTargetService(func(ctx context.Context, action func(context.Context, service.FileEntryPathReader) error) error {
			return store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, txStore *Store) error {
				return action(snapshotContext, &fileEntryPathChangingReader{Store: txStore, afterRead: func(id uint64) error {
					if id != 12 || changed {
						return nil
					}
					changed = true
					_, err := pool.ExecContext(snapshotContext, "UPDATE file_entries SET parent_id=15,revision=revision+1 WHERE id=14")
					return err
				}})
			})
		})
		result, err := concurrent.Check(ctx, actor, 12, 14)
		if err != nil || result == nil || !changed || len(result.TargetParent.Entries) != 2 || *result.TargetParent.Entries[1].ParentID != 11 {
			t.Fatal("同次读取混入并发目的地变化", err)
		}
		checkFailure(t, 12, 14, domain.ErrEntryMoveIntoSubtree)
	})
	t.Run("cancel and recover", func(t *testing.T) {
		callContext, cancelCall := context.WithCancel(ctx)
		defer cancelCall()
		cancelled := service.NewEntryMoveTargetService(func(ctx context.Context, action func(context.Context, service.FileEntryPathReader) error) error {
			return store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, txStore *Store) error {
				return action(snapshotContext, &fileEntryPathChangingReader{Store: txStore, afterRead: func(id uint64) error {
					if id == 13 {
						cancelCall()
					}
					return nil
				}})
			})
		})
		if result, err := cancelled.Check(callContext, actor, 13, 14); result != nil || !errors.Is(err, context.Canceled) {
			t.Fatal("取消未传播", err)
		}
		if result, err := checker.Check(ctx, actor, 13, 14); err != nil || result == nil {
			t.Fatal("取消后读取未恢复", err)
		}
	})
	t.Run("current path depth boundary", func(t *testing.T) {
		parent := uint64(11)
		for depth := 1; depth <= domain.MaxFileEntryDepth; depth++ {
			name, id := fmt.Sprintf("depth%d", depth), uint64(100+depth)
			if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,1,?,?,?,'DIR')", id, parent, name, []byte(name)); err != nil {
				t.Fatal(err)
			}
			parent = id
		}
		result, err := checker.Check(ctx, actor, 13, parent)
		// 返回当前 32 层目的地路径，后续服务据此校验移动后的最终深度。
		if err != nil || result == nil || len(result.TargetParent.Entries) != domain.MaxFileEntryDepth+1 {
			t.Fatal("当前路径边界错误", err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(133,1,?,'depth33',CAST('depth33' AS BINARY),'DIR')", parent); err != nil {
			t.Fatal(err)
		}
		checkFailure(t, 13, 133, domain.ErrInvalidFileEntryData)
	})
	t.Run("maximum unsigned identifiers", func(t *testing.T) {
		maximum := ^uint64(0)
		for offset, name := range []string{"高位源目录", "高位目的地"} {
			id := maximum - uint64(offset)
			if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,1,11,?,?,'DIR')", id, name, []byte(name)); err != nil {
				t.Fatal(err)
			}
		}
		result, err := checker.Check(ctx, actor, maximum, maximum-1)
		if err != nil || result == nil || result.Source.Entries[1].ID != maximum || result.TargetParent.Entries[1].ID != maximum-1 {
			t.Fatal("完整无符号标识错误", err)
		}
		checkFailure(t, maximum, maximum, domain.ErrEntryMoveIntoSubtree)
	})
}
