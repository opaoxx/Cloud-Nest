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

func TestWithPurgeRootTransactionIntegration(t *testing.T) {
	// 1. 用正式表建立清理中的根项，再由扫描入口产生候选。
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	pool.SetMaxOpenConns(2)
	startedAt := time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC)
	if _, err := pool.ExecContext(ctx, `INSERT INTO file_entries
		(id,user_id,parent_id,name,name_key,entry_type,status,purge_started_at,revision)
		VALUES(17,1,11,'清理中空目录',CAST('清理中空目录' AS BINARY),'DIR','PURGING',?,2)`, startedAt); err != nil {
		t.Fatal("准备清理根项夹具失败", err)
	}
	candidates, err := store.ListPurgeRootCandidates(ctx, 1)
	if err != nil || len(candidates) != 1 || candidates[0].EntryID != 17 {
		t.Fatal("未取得待清理根项候选", candidates, err)
	}
	candidate := candidates[0]

	// 2. 回调期间用另一连接试锁用户和根项，验证两把锁同时持有。
	t.Run("locks held until callback ends", func(t *testing.T) {
		err := store.WithPurgeRootTransaction(ctx, candidate, func(txContext context.Context, txStore *Store) error {
			if txStore.loginUserID != actor.UserID {
				return fmt.Errorf("事务 Store 没有绑定正确的用户")
			}
			contender, err := pool.BeginTx(txContext, nil)
			if err != nil {
				return err
			}
			defer contender.Rollback()
			for _, query := range []struct {
				sql string
				id  uint64
			}{
				{"SELECT id FROM users WHERE id=? FOR UPDATE NOWAIT", candidate.UserID},
				{"SELECT id FROM file_entries WHERE id=? FOR UPDATE NOWAIT", candidate.EntryID},
			} {
				var id uint64
				err := contender.QueryRowContext(txContext, query.sql, query.id).Scan(&id)
				var lockError *mysql.MySQLError
				if !errors.As(err, &lockError) || lockError.Number != 3572 {
					return fmt.Errorf("行 %d 未被事务锁住，试锁错误 %v", query.id, err)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		contender, err := pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer contender.Rollback()
		var id uint64
		if err := contender.QueryRowContext(ctx, "SELECT id FROM file_entries WHERE id=? FOR UPDATE NOWAIT", candidate.EntryID).Scan(&id); err != nil || id != candidate.EntryID {
			t.Fatal("事务结束后行锁未释放", id, err)
		}
	})

	// 3. 回调失败时回滚其写入，验证候选和进度保持原状。
	t.Run("callback failure rolls back", func(t *testing.T) {
		failure := errors.New("测试回调失败")
		err := store.WithPurgeRootTransaction(ctx, candidate, func(txContext context.Context, txStore *Store) error {
			if err := txStore.database.WithContext(txContext).Exec("UPDATE file_entries SET revision=revision+1 WHERE user_id=? AND id=?",
				candidate.UserID, candidate.EntryID).Error; err != nil {
				return err
			}
			return failure
		})
		if !errors.Is(err, failure) {
			t.Fatal("回滚未保留回调错误", err)
		}
		rows, err := store.ListPurgeRootCandidates(ctx, 1)
		if err != nil || !reflect.DeepEqual(rows, candidates) {
			t.Fatal("回调写入未回滚或复核改变了候选", rows, err)
		}
	})

	// 4. 无效、跨账号或已变化的候选均不得运行清理回调。
	unexpected := func(context.Context, *Store) error {
		t.Error("无效候选进入清理回调")
		return nil
	}
	for _, scenario := range []struct {
		name   string
		change func(*domain.PurgeRootCandidate)
		want   error
	}{
		{"invalid parent", func(row *domain.PurgeRootCandidate) { row.ParentID = row.EntryID }, domain.ErrInvalidFileEntryData},
		{"invalid precision", func(row *domain.PurgeRootCandidate) { row.PurgeStartedAt = row.PurgeStartedAt.Add(time.Nanosecond) }, domain.ErrInvalidFileEntryData},
		{"foreign entry", func(row *domain.PurgeRootCandidate) { row.UserID = 2 }, domain.ErrPurgeRootCandidateStale},
		{"missing entry", func(row *domain.PurgeRootCandidate) { row.EntryID = 99 }, domain.ErrPurgeRootCandidateStale},
		{"changed parent", func(row *domain.PurgeRootCandidate) { row.ParentID = 12 }, domain.ErrPurgeRootCandidateStale},
		{"changed revision", func(row *domain.PurgeRootCandidate) { row.Revision++ }, domain.ErrPurgeRootCandidateStale},
		{"changed start time", func(row *domain.PurgeRootCandidate) { row.PurgeStartedAt = row.PurgeStartedAt.Add(time.Microsecond) }, domain.ErrPurgeRootCandidateStale},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			stale := candidate
			scenario.change(&stale)
			if err := store.WithPurgeRootTransaction(ctx, stale, unexpected); !errors.Is(err, scenario.want) {
				t.Fatal("候选错误类别不符", err)
			}
		})
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if err := store.WithPurgeRootTransaction(canceled, candidate, unexpected); !errors.Is(err, context.Canceled) {
		t.Fatal("事务入口未保留取消原因", err)
	}

	// 5. 后台可继续处理已禁用账号的根项，完成提交后旧候选失效。
	if _, err := pool.ExecContext(ctx, "UPDATE users SET status='DISABLED' WHERE id=?", candidate.UserID); err != nil {
		t.Fatal(err)
	}
	calls := 0
	if err := store.WithPurgeRootTransaction(ctx, candidate, func(txContext context.Context, txStore *Store) error {
		calls++
		return txStore.database.WithContext(txContext).Exec("UPDATE file_entries SET status='PURGED',purged_at=? WHERE user_id=? AND id=?",
			startedAt.Add(time.Second), candidate.UserID, candidate.EntryID).Error
	}); err != nil || calls != 1 {
		t.Fatal("后台未处理已禁用账号的受理记录", calls, err)
	}
	if err := store.WithPurgeRootTransaction(ctx, candidate, unexpected); !errors.Is(err, domain.ErrPurgeRootCandidateStale) {
		t.Fatal("已完成根项仍可使用扫描候选", err)
	}
}
