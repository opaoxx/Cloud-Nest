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

func purgeLeafSettlementTestDatabase(t *testing.T, ctx context.Context) (*sql.DB, *Store) {
	t.Helper()
	pool, store, _ := trashEntryTestDatabase(t, ctx)
	installChangeEventFixtureTable(t, ctx, pool)
	for _, boundary := range []struct{ table, next string }{
		{"user_entry_preferences", "upload_tasks"},
		{"upload_tasks", "idempotency_records"},
	} {
		start := strings.Index(migrations.InitialSQL, "CREATE TABLE "+boundary.table+" (")
		end := strings.Index(migrations.InitialSQL, "CREATE TABLE "+boundary.next+" (")
		if start < 0 || end <= start {
			t.Fatal("结算夹具表迁移未找到", boundary.table)
		}
		if _, err := pool.ExecContext(ctx, migrations.InitialSQL[start:end]); err != nil {
			t.Fatal("安装结算夹具表失败", boundary.table, err)
		}
	}
	if _, err := pool.ExecContext(ctx, `UPDATE stored_objects SET
		status='READY',verified_sha256=UNHEX(REPEAT('41',32)),verified_at=UTC_TIMESTAMP(6) WHERE id=41`); err != nil {
		t.Fatal("准备已发布的测试对象失败", err)
	}
	return pool, store
}

func TestCompletePurgeLeafIntegration(t *testing.T) {
	// 1. 准备共享对象的两个文件、账户账目和正式偏好表。
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	pool, store := purgeLeafSettlementTestDatabase(t, ctx)
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal("执行单项清理夹具语句失败", err)
		}
	}
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
		status                                     string
		revision, used, reserved, sequence, events uint64
		objectID                                   sql.Null[uint64]
		completedAt                                sql.NullTime
		preferenceCount                            int
	}
	readState := func(entryID uint64) settlementState {
		t.Helper()
		var state settlementState
		err := pool.QueryRowContext(ctx, `
			SELECT entry.status,entry.revision,entry.object_id,entry.purged_at,account.used_storage,account.reserved_storage,account.change_seq,
			       (SELECT COUNT(*) FROM user_entry_preferences WHERE user_id=entry.user_id AND entry_id=entry.id),
			       (SELECT COUNT(*) FROM change_events WHERE user_id=entry.user_id)
			FROM file_entries AS entry JOIN users AS account ON account.id=entry.user_id
			WHERE entry.user_id=1 AND entry.id=?`, entryID).
			Scan(&state.status, &state.revision, &state.objectID, &state.completedAt, &state.used, &state.reserved, &state.sequence,
				&state.preferenceCount, &state.events)
		if err != nil {
			t.Fatal("读取结算后的数据库状态失败", err)
		}
		return state
	}
	before := readState(15)

	// 2. 让删除偏好的最后一步报错，回查早先写入的配额和目录项。
	execute(`CREATE TRIGGER fixture_fail_purge_preference AFTER DELETE ON user_entry_preferences
		FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'fixture purge preference failure'`)
	var databaseError *mysql.MySQLError
	if err := settle(); !errors.As(err, &databaseError) || databaseError.Number != 1644 ||
		databaseError.Message != "fixture purge preference failure" {
		t.Fatal("没有收到偏好删除失败的原始原因", err)
	}
	if after := readState(15); after != before {
		t.Fatalf("失败后仍有结算写入，原状态 %+v，当前 %+v", before, after)
	}
	execute("DROP TRIGGER fixture_fail_purge_preference")

	// 3. 重试成功后从事务外核对三项写入，并确定共享内容仍可被其他文件引用。
	if err := settle(); err != nil {
		t.Fatal("单项清理结算失败", err)
	}
	committed := readState(15)
	if committed.status != "PURGED" || committed.revision != candidate.Revision+1 ||
		committed.objectID.Valid || !committed.completedAt.Valid || committed.used != 2048 ||
		committed.reserved != 512 || committed.preferenceCount != 0 || committed.sequence != 1 || committed.events != 1 {
		t.Fatalf("单项清理结算结果不符：%+v", committed)
	}
	var sharedReferenceCount int
	err = pool.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM file_entries AS entry
		JOIN stored_objects AS object ON object.id=entry.object_id
		JOIN user_entry_preferences AS preference ON preference.user_id=entry.user_id AND preference.entry_id=entry.id
		WHERE entry.user_id=1 AND entry.id=13 AND entry.status='ACTIVE' AND object.id=41
		  AND object.unreferenced_at IS NULL AND object.gc_after IS NULL`).
		Scan(&sharedReferenceCount)
	if err != nil || sharedReferenceCount != 1 {
		t.Fatal("范围外的文件、偏好或共享对象发生变化", sharedReferenceCount, err)
	}

	// 4. 再用原候选结算时，复核挡住重复扣减。
	if err := settle(); !errors.Is(err, domain.ErrPurgeLeafCandidateStale) {
		t.Fatal("已完成叶子的原候选应该失效", err)
	}
	if after := readState(15); after != committed {
		t.Fatalf("重复使用原候选改动了已提交的结算：%+v", after)
	}

	// 5. 受理共享对象的最后一个文件，验证计时写入失败会撤销文件结算。
	if count, err := store.CompletePurgeLeafBatch(ctx, roots[0], 1); err != nil || count != 1 {
		t.Fatal("首个根目录收尾失败", count, err)
	}
	execute(`UPDATE file_entries SET status='PURGING',purge_started_at=UTC_TIMESTAMP(6),
		deleted_at=UTC_TIMESTAMP(6),revision=revision+1 WHERE id=13`)
	lastRoots, err := store.ListPurgeRootCandidates(ctx, 1)
	if err != nil || len(lastRoots) != 1 || lastRoots[0].EntryID != 13 {
		t.Fatal("未取得最后一个文件的清理根项", lastRoots, err)
	}
	lastRoot := lastRoots[0]
	beforeLast := readState(13)
	assertLastRolledBack := func() {
		t.Helper()
		object, err := store.FindUserStoredObject(ctx, 1, 41)
		if err != nil || object.UnreferencedAt != nil || object.GCAfter != nil || readState(13) != beforeLast {
			t.Fatal("失败后文件结算或对象计时未一起回滚", object, err)
		}
	}
	execute(`CREATE TRIGGER fixture_fail_purge_gc_timer BEFORE UPDATE ON stored_objects
		FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'fixture purge GC timer failure'`)
	count, err := store.CompletePurgeLeafBatch(ctx, lastRoot, 1)
	if count != 0 || !errors.As(err, &databaseError) || databaseError.Number != 1644 ||
		databaseError.Message != "fixture purge GC timer failure" {
		t.Fatal("计时失败应保留原始错误并返回零项", count, err)
	}
	assertLastRolledBack()
	execute("DROP TRIGGER fixture_fail_purge_gc_timer")

	// 6. 验证已有计时和事件也会随外层事务回滚。
	rollbackFixture := errors.New("fixture rolls back purge settlement and timer")
	err = store.WithPurgeRootTransaction(ctx, lastRoot, func(txContext context.Context, txStore *Store) error {
		candidates, err := txStore.ListPurgeLeafCandidates(txContext, 1)
		if err != nil {
			return err
		}
		if len(candidates) != 1 || candidates[0].EntryID != lastRoot.EntryID {
			return fmt.Errorf("最后一个文件候选不符：%+v", candidates)
		}
		if err := txStore.CompletePurgeLeaf(txContext, candidates[0]); err != nil {
			return err
		}
		object, err := txStore.FindUserStoredObject(txContext, 1, 41)
		if err != nil || object.UnreferencedAt == nil || object.GCAfter == nil {
			return fmt.Errorf("事务内没有读到已启动的计时：%v", err)
		}
		var sequence, events uint64
		if err := txStore.database.WithContext(txContext).Raw(`SELECT change_seq,
			(SELECT COUNT(*) FROM change_events WHERE user_id=users.id) FROM users WHERE id=1`).
			Row().Scan(&sequence, &events); err != nil || sequence != 3 || events != 3 {
			return fmt.Errorf("事务内最后一项未写入序号和事件，序号 %d，事件 %d，错误 %v", sequence, events, err)
		}
		return rollbackFixture
	})
	if !errors.Is(err, rollbackFixture) {
		t.Fatal("未验证已写入计时的回滚", err)
	}
	assertLastRolledBack()

	// 7. 重试并提交，文件结算与七天计时均可在事务外读取。
	if count, err := store.CompletePurgeLeafBatch(ctx, lastRoot, 1); err != nil || count != 1 {
		t.Fatal("最后一个文件结算失败", count, err)
	}
	last := readState(13)
	object, err := store.FindUserStoredObject(ctx, 1, 41)
	if err != nil || object.Status != domain.StoredObjectStatusReady || object.UnreferencedAt == nil || object.GCAfter == nil ||
		!object.GCAfter.Equal(object.UnreferencedAt.Add(7*24*time.Hour)) ||
		last.status != "PURGED" || last.used != 0 || last.reserved != 512 || last.objectID.Valid || last.preferenceCount != 0 ||
		last.sequence != 3 || last.events != 3 {
		t.Fatal("最后一个文件的结算或对象回收计时不符", last, object, err)
	}
	t.Log("最后一个引用的完成事件和七天回收计时一起回滚，重试一起提交")
}
