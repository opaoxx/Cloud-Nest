package repository

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestWithFileEntrySnapshotIntegration(t *testing.T) {
	// 1. 复用正式目录项表和独占库，建立父目录与子项。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(2)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash) VALUES(1,'fixture_one','快照用户','fixture_hash'),(3,'fixture_three','其他用户','fixture_hash')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(33,3,NULL,'',X'','DIR'),(12,1,11,'原目录',CAST('原目录' AS BINARY),'DIR'),(14,1,11,'新目录',CAST('新目录' AS BINARY),'DIR'),(13,1,12,'子项',CAST('子项' AS BINARY),'DIR'),(18,1,11,'首次读取测试',CAST('首次读取测试' AS BINARY),'DIR')",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("创建快照夹具失败", err)
		}
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	readEntry := func(t *testing.T, readContext context.Context, reader *Store, entryID uint64) *domain.FileEntry {
		t.Helper()
		entry, err := reader.FindUserFileEntry(readContext, 1, entryID)
		if err != nil || entry == nil {
			t.Fatal("读取快照目录项失败", err)
		}
		return entry
	}

	// 2. 将读取连接的默认隔离级别改为 READ COMMITTED，验证入口显式选用可重复读。
	t.Run("repeatable snapshot across committed move and trash", func(t *testing.T) {
		writer, err := pool.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer writer.Close()
		reader, err := pool.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := reader.ExecContext(ctx, "SET SESSION TRANSACTION ISOLATION LEVEL READ COMMITTED"); err != nil {
			_ = reader.Close()
			t.Fatal(err)
		}
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
		err = store.WithFileEntrySnapshot(ctx, func(txContext context.Context, txStore *Store) error {
			// 首次读取子项建立快照，再由独立连接提交移动与原父目录回收。
			first := readEntry(t, txContext, txStore, 13)
			if first.ParentID == nil || *first.ParentID != 12 {
				t.Fatal("首次读取的父目录错误")
			}
			writeContext, release := context.WithTimeout(ctx, time.Second)
			defer release()
			transaction, err := writer.BeginTx(writeContext, nil)
			if err != nil {
				return err
			}
			defer transaction.Rollback()
			if _, err := transaction.ExecContext(writeContext, "UPDATE file_entries SET parent_id=14,revision=revision+1,modified_at=UTC_TIMESTAMP(6),updated_at=UTC_TIMESTAMP(6) WHERE id=13"); err != nil {
				return err
			}
			if _, err := transaction.ExecContext(writeContext, "UPDATE file_entries SET status='TRASHED',deleted_at=UTC_TIMESTAMP(6),revision=revision+1,modified_at=UTC_TIMESTAMP(6),updated_at=UTC_TIMESTAMP(6) WHERE id=12"); err != nil {
				return err
			}
			if err := transaction.Commit(); err != nil {
				return err
			}
			parent := readEntry(t, txContext, txStore, 12)
			again := readEntry(t, txContext, txStore, 13)
			if parent.Status != "ACTIVE" || parent.Revision != 1 || !reflect.DeepEqual(first, again) {
				t.Fatal("快照混用了移动/回收前后的目录事实")
			}
			if result, err := txStore.FindUserFileEntry(txContext, 1, 33); result != nil || !errors.Is(err, domain.ErrFileEntryNotFound) {
				t.Fatal("快照读取丢失用户归属条件", err)
			}
			return nil
		})
		if err != nil {
			t.Fatal("显式可重复读快照失败", err)
		}
		if err := store.WithFileEntrySnapshot(ctx, func(txContext context.Context, txStore *Store) error {
			child, parent := readEntry(t, txContext, txStore, 13), readEntry(t, txContext, txStore, 12)
			if child.ParentID == nil || *child.ParentID != 14 || child.Revision != 2 || parent.Status != "TRASHED" || parent.Revision != 2 {
				t.Fatal("新快照未取得已提交移动与回收")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("first consistent read establishes snapshot", func(t *testing.T) {
		if err := store.WithFileEntrySnapshot(ctx, func(txContext context.Context, txStore *Store) error {
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET name='first',name_key=CAST('first' AS BINARY),revision=revision+1 WHERE id=18"); err != nil {
				return err
			}
			first := readEntry(t, txContext, txStore, 18)
			if first.Name != "first" {
				t.Fatal("首次查询未取得开启事务后已提交的记录")
			}
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET name='second',name_key=CAST('second' AS BINARY),revision=revision+1 WHERE id=18"); err != nil {
				return err
			}
			if again := readEntry(t, txContext, txStore, 18); !reflect.DeepEqual(first, again) {
				t.Fatal("首次读取后快照发生变化")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if current := readEntry(t, ctx, store, 18); current.Name != "second" {
			t.Fatal("事务结束后未看到最新提交记录")
		}
	})

	// 3. 真实 MySQL 拒绝持久表写入，错误后仍能重新开启正常快照。
	t.Run("database rejects persistent writes", func(t *testing.T) {
		before := readEntry(t, ctx, store, 13)
		err := store.WithFileEntrySnapshot(ctx, func(txContext context.Context, txStore *Store) error {
			return txStore.database.WithContext(txContext).Exec("UPDATE file_entries SET revision=revision+1 WHERE id=13").Error
		})
		var databaseError *mysql.MySQLError
		if !errors.As(err, &databaseError) || databaseError.Number != 1792 {
			t.Fatal("持久表写入未按只读事务拒绝", err)
		}
		if after := readEntry(t, ctx, store, 13); !reflect.DeepEqual(before, after) {
			t.Fatal("只读事务写入改变了目录项")
		}
	})
	t.Run("callback error panic cancellation and deadline release resources", func(t *testing.T) {
		for _, mode := range []string{"error", "panic", "cancel", "deadline"} {
			t.Run(mode, func(t *testing.T) {
				requestContext, stop := context.WithCancel(ctx)
				defer stop()
				if mode == "deadline" {
					var release context.CancelFunc
					requestContext, release = context.WithTimeout(ctx, 30*time.Millisecond)
					defer release()
				}
				failure := errors.New("fixture callback")
				action := func(txContext context.Context, txStore *Store) error {
					_ = readEntry(t, txContext, txStore, 13)
					switch mode {
					case "panic":
						panic(failure)
					case "cancel":
						stop()
						return nil
					case "deadline":
						<-txContext.Done()
						return nil
					default:
						return failure
					}
				}
				if mode == "panic" {
					func() {
						defer func() {
							if recover() != failure {
								t.Error("快照吞掉了原始 panic")
							}
						}()
						_ = store.WithFileEntrySnapshot(requestContext, action)
					}()
				} else {
					wantError := failure
					if mode == "cancel" {
						wantError = context.Canceled
					}
					if mode == "deadline" {
						wantError = context.DeadlineExceeded
					}
					if err := store.WithFileEntrySnapshot(requestContext, action); !errors.Is(err, wantError) {
						t.Fatal("快照故障分类错误", err)
					}
				}
				if err := store.WithFileEntrySnapshot(ctx, func(txContext context.Context, txStore *Store) error {
					_ = readEntry(t, txContext, txStore, 13)
					return nil
				}); err != nil {
					t.Fatal("故障后未能复用连接建立新快照", err)
				}
			})
		}
	})
	t.Run("nested entry and escaped store rejected", func(t *testing.T) {
		var captured *Store
		var capturedContext context.Context
		err := store.WithFileEntrySnapshot(ctx, func(txContext context.Context, txStore *Store) error {
			captured, capturedContext = txStore, txContext
			if err := txStore.WithFileEntrySnapshot(txContext, func(context.Context, *Store) error { t.Error("执行了嵌套读取"); return nil }); err == nil {
				t.Fatal("嵌套快照未被拒绝")
			}
			if err := txStore.WithLoginUserTransaction(txContext, 1, func(context.Context, *Store) error { t.Error("执行了嵌套写入"); return nil }); err == nil {
				t.Fatal("快照内嵌套写事务未被拒绝")
			}
			return nil
		})
		if err != nil || captured == nil || capturedContext.Err() == nil {
			t.Fatal("快照结束或上下文状态错误", err)
		}
		if _, err := captured.FindUserFileEntry(ctx, 1, 13); !errors.Is(err, sql.ErrTxDone) {
			t.Fatal("回调结束后仍可使用事务 Store", err)
		}
	})
}
