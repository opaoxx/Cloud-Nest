package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestWithUserSyncSnapshotIntegration(t *testing.T) {
	// 1. 使用独占库准备两个活动根目录、一个禁用账号和一个缺失根目录的账号。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(2)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash,status,change_seq) VALUES (1,'snapshot_one','Owner','fixture_hash','ACTIVE',0),(2,'snapshot_two','Other owner','fixture_hash','ACTIVE',18446744073709551615),(3,'snapshot_disabled','Disabled','fixture_hash','DISABLED',0),(4,'snapshot_missing_root','Missing root','fixture_hash','ACTIVE',0)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES (11,1,NULL,'',X'','DIR'),(22,2,NULL,'',X'','DIR'),(33,3,NULL,'',X'','DIR'),(12,1,11,'original',CAST('original' AS BINARY),'DIR')",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("创建同步快照夹具失败", err)
		}
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}

	// 2. 核对归属、零序号和完整无符号序号，拒绝交付无法构建基线的账号。
	for _, testCase := range []struct {
		name   string
		userID uint64
		header domain.SyncSnapshotHeader
		err    error
	}{
		{name: "zero sequence", userID: 1, header: domain.SyncSnapshotHeader{RootEntryID: 11, HighWaterSequence: 0}},
		{name: "other root and maximum sequence", userID: 2, header: domain.SyncSnapshotHeader{RootEntryID: 22, HighWaterSequence: ^uint64(0)}},
		{name: "disabled account", userID: 3, err: domain.ErrAccountDisabled},
		{name: "missing root", userID: 4, err: domain.ErrInvalidAccountData},
		{name: "missing account", userID: 99, err: domain.ErrAccountNotFound},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			called := false
			err := store.WithUserSyncSnapshot(ctx, testCase.userID, func(_ context.Context, _ *Store, header domain.SyncSnapshotHeader) error {
				called = true
				if header != testCase.header {
					t.Fatalf("头部 = %+v，预期 %+v", header, testCase.header)
				}
				return nil
			})
			if !errors.Is(err, testCase.err) || called != (testCase.err == nil) {
				t.Fatalf("回调执行=%t，错误=%v，预期错误=%v", called, err, testCase.err)
			}
		})
	}

	// 3. 让独立连接在头部读取后同时提交序号和目录改名，再比较新旧读取视图。
	t.Run("header pins the view before the first entry read", func(t *testing.T) {
		writer, err := pool.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer writer.Close()
		checkEntry := func(readContext context.Context, reader *Store, expectedName string, expectedRevision uint64) {
			t.Helper()
			entry, err := reader.FindUserFileEntry(readContext, 1, 12)
			if err != nil || entry == nil || entry.Name != expectedName || entry.Revision != expectedRevision {
				t.Fatalf("读取目录项 = %+v，错误 = %v，预期名称 %s 版本 %d", entry, err, expectedName, expectedRevision)
			}
		}
		err = store.WithUserSyncSnapshot(ctx, 1, func(readContext context.Context, reader *Store, header domain.SyncSnapshotHeader) error {
			if header != (domain.SyncSnapshotHeader{RootEntryID: 11, HighWaterSequence: 0}) {
				t.Fatalf("首次头部 = %+v，预期根目录 11 和序号 0", header)
			}
			writeContext, stop := context.WithTimeout(ctx, 2*time.Second)
			defer stop()
			transaction, err := writer.BeginTx(writeContext, nil)
			if err != nil {
				return err
			}
			defer transaction.Rollback()
			if _, err := transaction.ExecContext(writeContext, "UPDATE users SET change_seq=1 WHERE id=1"); err != nil {
				return err
			}
			if _, err := transaction.ExecContext(writeContext, "UPDATE file_entries SET name='renamed',name_key=CAST('renamed' AS BINARY),revision=2 WHERE id=12"); err != nil {
				return err
			}
			if err := transaction.Commit(); err != nil {
				return err
			}
			again, err := reader.findUserSyncSnapshotHeader(readContext, 1)
			if err != nil || again != header {
				t.Fatalf("旧视图头部变化，首次=%+v，再次=%+v，错误=%v", header, again, err)
			}
			checkEntry(readContext, reader, "original", 1)
			return nil
		})
		if err != nil {
			t.Fatal("旧视图读取失败", err)
		}
		if err := store.WithUserSyncSnapshot(ctx, 1, func(readContext context.Context, reader *Store, header domain.SyncSnapshotHeader) error {
			if header != (domain.SyncSnapshotHeader{RootEntryID: 11, HighWaterSequence: 1}) {
				t.Fatalf("新视图头部 = %+v，预期根目录 11 和序号 1", header)
			}
			checkEntry(readContext, reader, "renamed", 2)
			return nil
		}); err != nil {
			t.Fatal("新视图读取失败", err)
		}
	})
}
