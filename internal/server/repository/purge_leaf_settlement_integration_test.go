package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestCompletePurgeLeafIntegration(t *testing.T) {
	// 1. 准备共享对象的两个文件、账户账目和正式偏好表。
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	pool, store, _ := trashEntryTestDatabase(t, ctx)
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal("执行单项清理夹具语句失败", err)
		}
	}
	preferencesStart := strings.Index(migrations.InitialSQL, "CREATE TABLE user_entry_preferences (")
	preferencesEnd := strings.Index(migrations.InitialSQL, "CREATE TABLE upload_tasks (")
	if preferencesStart < 0 || preferencesEnd <= preferencesStart {
		t.Fatal("目录项偏好表迁移未找到")
	}
	execute(migrations.InitialSQL[preferencesStart:preferencesEnd])
	execute("UPDATE file_entries SET status='PURGING',purge_started_at=UTC_TIMESTAMP(6),revision=revision+1 WHERE id=14")
	execute("UPDATE users SET storage_quota=10000,used_storage=4096,reserved_storage=512 WHERE id=1")
	execute(`INSERT INTO user_entry_preferences(user_id,entry_id,starred_at,last_opened_at)
		VALUES (1,15,UTC_TIMESTAMP(6),UTC_TIMESTAMP(6)), (1,13,UTC_TIMESTAMP(6),NULL)`)

	roots, err := store.ListPurgeRootCandidates(ctx, 1)
	if err != nil || len(roots) != 1 || roots[0].EntryID != 14 {
		t.Fatal("未取得单项清理夹具的根项", roots, err)
	}
	var candidate domain.PurgeLeafCandidate
	err = store.WithPurgeRootTransaction(ctx, roots[0], func(txContext context.Context, txStore *Store) error {
		candidates, err := txStore.ListPurgeLeafCandidates(txContext, 10)
		if err != nil {
			return err
		}
		if len(candidates) != 1 || candidates[0].EntryID != 15 || candidates[0].FileSize != 2048 {
			return fmt.Errorf("单项清理夹具的叶子候选不符：%+v", candidates)
		}
		candidate = candidates[0]
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	settle := func() error {
		return store.WithPurgeRootTransaction(ctx, roots[0], func(txContext context.Context, txStore *Store) error {
			return txStore.CompletePurgeLeaf(txContext, candidate)
		})
	}
	type settlementState struct {
		status                   string
		revision, used, reserved uint64
		objectID                 sql.Null[uint64]
		completedAt              sql.NullTime
		preferenceCount          int
	}
	readState := func() settlementState {
		t.Helper()
		var state settlementState
		err := pool.QueryRowContext(ctx, `
			SELECT entry.status,entry.revision,entry.object_id,entry.purged_at,account.used_storage,account.reserved_storage,
			       (SELECT COUNT(*) FROM user_entry_preferences WHERE user_id=entry.user_id AND entry_id=entry.id)
			FROM file_entries AS entry JOIN users AS account ON account.id=entry.user_id
			WHERE entry.user_id=1 AND entry.id=15`).
			Scan(&state.status, &state.revision, &state.objectID, &state.completedAt, &state.used, &state.reserved, &state.preferenceCount)
		if err != nil {
			t.Fatal("读取结算后的数据库状态失败", err)
		}
		return state
	}
	before := readState()

	// 2. 让删除偏好的最后一步报错，回查早先写入的配额和目录项。
	execute(`CREATE TRIGGER fixture_fail_purge_preference AFTER DELETE ON user_entry_preferences
		FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'fixture purge preference failure'`)
	var databaseError *mysql.MySQLError
	if err := settle(); !errors.As(err, &databaseError) || databaseError.Number != 1644 ||
		databaseError.Message != "fixture purge preference failure" {
		t.Fatal("没有收到偏好删除失败的原始原因", err)
	}
	if after := readState(); after != before {
		t.Fatalf("失败后仍有结算写入，原状态 %+v，当前 %+v", before, after)
	}
	execute("DROP TRIGGER fixture_fail_purge_preference")

	// 3. 重试成功后从事务外核对三项写入，并确定共享内容仍可被其他文件引用。
	if err := settle(); err != nil {
		t.Fatal("单项清理结算失败", err)
	}
	committed := readState()
	if committed.status != "PURGED" || committed.revision != candidate.Revision+1 ||
		committed.objectID.Valid || !committed.completedAt.Valid || committed.used != 2048 ||
		committed.reserved != 512 || committed.preferenceCount != 0 {
		t.Fatalf("单项清理结算结果不符：%+v", committed)
	}
	var sharedReferenceCount int
	err = pool.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM file_entries AS entry
		JOIN stored_objects AS object ON object.id=entry.object_id
		JOIN user_entry_preferences AS preference ON preference.user_id=entry.user_id AND preference.entry_id=entry.id
		WHERE entry.user_id=1 AND entry.id=13 AND entry.status='ACTIVE' AND object.id=41`).
		Scan(&sharedReferenceCount)
	if err != nil || sharedReferenceCount != 1 {
		t.Fatal("范围外的文件、偏好或共享对象发生变化", sharedReferenceCount, err)
	}

	// 4. 再用原候选结算时，复核挡住重复扣减。
	if err := settle(); !errors.Is(err, domain.ErrPurgeLeafCandidateStale) {
		t.Fatal("已完成叶子的原候选应该失效", err)
	}
	if after := readState(); after != committed {
		t.Fatalf("重复使用原候选改动了已提交的结算：%+v", after)
	}
}
