package repository

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestWithExpiredTrashRootTransactionIntegration(t *testing.T) {
	// 1. 由正式扫描取得一条经过多层活动目录的到期候选。
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	pool, store, _ := trashEntryTestDatabase(t, ctx)
	pool.SetMaxOpenConns(2)
	execute := func(t *testing.T, statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal("准备复核夹具失败", err)
		}
	}
	var now time.Time
	if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); err != nil {
		t.Fatal(err)
	}
	old := now.Truncate(time.Second).AddDate(0, 0, -31)
	execute(t, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES
		(17,1,12,'复核父目录',CAST('复核父目录' AS BINARY),'DIR'),
		(18,1,11,'活动新位置',CAST('活动新位置' AS BINARY),'DIR')`)
	execute(t, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at,revision)
		VALUES(27,1,17,'到期测试项',CAST('到期测试项' AS BINARY),'DIR','TRASHED',?,4)`, old)
	candidates, err := store.ListExpiredTrashRootCandidates(ctx, 100)
	if err != nil || len(candidates) != 1 || candidates[0].EntryID != 27 {
		t.Fatal("未取得预期根项候选", candidates, err)
	}
	candidate := candidates[0]

	// 2. 回调期间用另一条连接试锁用户、目标和各层祖先。
	t.Run("holds locks through callback", func(t *testing.T) {
		err := store.WithExpiredTrashRootTransaction(ctx, candidate, func(txContext context.Context, txStore *Store, current domain.ExpiredTrashRootCandidate) error {
			if txStore.loginUserID != candidate.UserID || current != candidate {
				return fmt.Errorf("回调未收到所属用户事务和当前候选")
			}
			contender, err := pool.BeginTx(txContext, nil)
			if err != nil {
				return err
			}
			defer contender.Rollback()
			for _, probe := range []struct {
				statement string
				id        uint64
			}{
				{"SELECT id FROM users WHERE id=? FOR UPDATE NOWAIT", candidate.UserID},
				{"SELECT id FROM file_entries WHERE id=? FOR UPDATE NOWAIT", candidate.EntryID},
				{"SELECT id FROM file_entries WHERE id=? FOR UPDATE NOWAIT", candidate.ParentID},
				{"SELECT id FROM file_entries WHERE id=? FOR UPDATE NOWAIT", 12},
				{"SELECT id FROM file_entries WHERE id=? FOR UPDATE NOWAIT", 11},
			} {
				var id uint64
				var lockError *mysql.MySQLError
				err := contender.QueryRowContext(txContext, probe.statement, probe.id).Scan(&id)
				if !errors.As(err, &lockError) || lockError.Number != 3572 {
					return fmt.Errorf("回调期间行 %d 未持有排他锁: %v", probe.id, err)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		var id uint64
		if err := pool.QueryRowContext(ctx, "SELECT id FROM file_entries WHERE id=27 FOR UPDATE NOWAIT").Scan(&id); err != nil || id != candidate.EntryID {
			t.Fatal("事务结束后未释放目标行锁", id, err)
		}
		after, err := store.ListExpiredTrashRootCandidates(ctx, 100)
		if err != nil || !reflect.DeepEqual(after, candidates) {
			t.Fatal("复核改变了候选事实", after, err)
		}
	})

	// 3. 模拟扫描之后的恢复、移动、再次回收或版本变化。
	unexpected := func(context.Context, *Store, domain.ExpiredTrashRootCandidate) error {
		t.Error("失效候选进入了回调")
		return nil
	}
	checkStale := func(t *testing.T, input domain.ExpiredTrashRootCandidate) {
		t.Helper()
		if err := store.WithExpiredTrashRootTransaction(ctx, input, unexpected); !errors.Is(err, domain.ErrExpiredTrashRootCandidateStale) {
			t.Fatal("应返回候选失效", err)
		}
	}
	for _, fixture := range []struct {
		name       string
		change     func(*testing.T)
		restoreSQL string
		restoreArg []any
	}{
		{
			name: "restored after scan",
			change: func(t *testing.T) {
				execute(t, "UPDATE file_entries SET status='ACTIVE', deleted_at=NULL, revision=5 WHERE id=27")
			},
			restoreSQL: "UPDATE file_entries SET status='TRASHED', deleted_at=?, revision=4 WHERE id=27",
			restoreArg: []any{old},
		},
		{
			name: "target parent changed",
			change: func(t *testing.T) {
				execute(t, "UPDATE file_entries SET parent_id=18 WHERE id=27")
			},
			restoreSQL: "UPDATE file_entries SET parent_id=17 WHERE id=27",
		},
		{
			name: "ancestor moved below trashed directory",
			change: func(t *testing.T) {
				execute(t, "UPDATE file_entries SET parent_id=14 WHERE id=12")
			},
			restoreSQL: "UPDATE file_entries SET parent_id=11 WHERE id=12",
		},
		{
			name: "ancestor entered purge",
			change: func(t *testing.T) {
				execute(t, "UPDATE file_entries SET status='PURGING', purge_started_at=UTC_TIMESTAMP(6) WHERE id=12")
			},
			restoreSQL: "UPDATE file_entries SET status='ACTIVE', purge_started_at=NULL WHERE id=12",
		},
		{
			name: "revision changed",
			change: func(t *testing.T) {
				execute(t, "UPDATE file_entries SET revision=5 WHERE id=27")
			},
			restoreSQL: "UPDATE file_entries SET revision=4 WHERE id=27",
		},
		{
			name: "deletion time changed but still expired",
			change: func(t *testing.T) {
				execute(t, "UPDATE file_entries SET deleted_at=? WHERE id=27", old.Add(time.Microsecond))
			},
			restoreSQL: "UPDATE file_entries SET deleted_at=? WHERE id=27",
			restoreArg: []any{old},
		},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			fixture.change(t)
			t.Cleanup(func() { execute(t, fixture.restoreSQL, fixture.restoreArg...) })
			checkStale(t, candidate)
		})
	}
	foreign := candidate
	foreign.UserID = 2
	checkStale(t, foreign)
	canceledContext, stop := context.WithCancel(ctx)
	stop()
	if err := store.WithExpiredTrashRootTransaction(canceledContext, candidate, unexpected); !errors.Is(err, context.Canceled) {
		t.Fatal("未保留复核取消原因", err)
	}

	// 4. 让独占连接上的当前时刻恰好到达保留截止点，验证复核重算到期条件。
	t.Run("exact cutoff is still retained", func(t *testing.T) {
		pool.SetMaxIdleConns(1)
		pool.SetMaxOpenConns(1)
		deadline := old.AddDate(0, 0, domain.TrashRetentionDays)
		execute(t, "SET timestamp = ?", deadline.Unix())
		t.Cleanup(func() { execute(t, "SET timestamp = DEFAULT") })
		var databaseTime time.Time
		if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&databaseTime); err != nil || !databaseTime.Equal(deadline) {
			t.Fatal("未固定独占连接的数据库时钟", databaseTime, err)
		}
		checkStale(t, candidate)
	})
}
