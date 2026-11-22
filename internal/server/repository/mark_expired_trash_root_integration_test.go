package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
)

func TestMarkExpiredTrashRootPurgingIntegration(t *testing.T) {
	// 1. 准备实际扫描候选和未满足条件的对照项。
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	pool, store, _ := trashEntryTestDatabase(t, ctx)
	installChangeEventFixtureTable(t, ctx, pool)
	pool.SetMaxOpenConns(2)
	execute := func(t *testing.T, statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal("准备到期标记夹具失败", err)
		}
	}
	var now time.Time
	if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); err != nil {
		t.Fatal(err)
	}
	old, young := now.AddDate(0, 0, -31), now.AddDate(0, 0, -29)
	execute(t, "UPDATE users SET used_storage=4096 WHERE id=1")
	execute(t, "UPDATE file_entries SET status='TRASHED',deleted_at=? WHERE user_id=1 AND id IN (13,15,16)", old)
	execute(t, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at,revision) VALUES
		(17,1,11,'待恢复目录',CAST('待恢复目录' AS BINARY),'DIR','TRASHED',?,2),
		(18,1,11,'尚未到期目录',CAST('尚未到期目录' AS BINARY),'DIR','TRASHED',?,2)`, old, young)
	scanned, err := store.ListExpiredTrashRootCandidates(ctx, 100)
	if err != nil || len(scanned) != 3 || scanned[0].EntryID != 13 || scanned[1].EntryID != 16 || scanned[2].EntryID != 17 {
		t.Fatal("未取得三条预期到期候选", scanned, err)
	}
	target, exhausted, restored := scanned[0], scanned[1], scanned[2]
	if exhausted.Revision != math.MaxUint64 {
		t.Fatal("夹具应保留最大版本测试项", exhausted)
	}
	execute(t, "UPDATE file_entries SET status='ACTIVE',deleted_at=NULL,revision=3 WHERE id=17")

	read := func(t *testing.T, id uint64) models.FileEntryTrash {
		t.Helper()
		var row models.FileEntryTrash
		if err := store.database.WithContext(ctx).Where("id = ?", id).Take(&row).Error; err != nil {
			t.Fatal("读回夹具状态失败", id, err)
		}
		return row
	}
	checkLog := func(t *testing.T, want uint64) {
		t.Helper()
		var sequence, events uint64
		if err := pool.QueryRowContext(ctx, `SELECT change_seq,
			(SELECT COUNT(*) FROM change_events WHERE user_id=users.id) FROM users WHERE id=1`).
			Scan(&sequence, &events); err != nil || sequence != want || events != want {
			t.Fatal("到期标记的账号序号或事件数不符", sequence, events, err)
		}
	}
	checkLog(t, 0)

	// 2. 失效、未到期、被祖先覆盖及版本耗尽的项均不能写入。
	oldRevision := target
	oldRevision.Revision--
	for _, fixture := range []struct {
		name      string
		candidate domain.ExpiredTrashRootCandidate
		want      error
	}{
		{"扫描后已恢复", restored, domain.ErrExpiredTrashRootCandidateStale},
		{"未到期", domain.ExpiredTrashRootCandidate{EntryID: 18, UserID: 1, ParentID: 11, Revision: 2, DeletedAt: young}, domain.ErrExpiredTrashRootCandidateStale},
		{"祖先已在回收站", domain.ExpiredTrashRootCandidate{EntryID: 15, UserID: 1, ParentID: 14, Revision: 5, DeletedAt: old}, domain.ErrExpiredTrashRootCandidateStale},
		{"旧版本", oldRevision, domain.ErrExpiredTrashRootCandidateStale},
		{"最大版本", exhausted, domain.ErrEntryRevisionExhausted},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			before := read(t, fixture.candidate.EntryID)
			result, err := store.MarkExpiredTrashRootPurging(ctx, fixture.candidate)
			if result != nil || !errors.Is(err, fixture.want) || !reflect.DeepEqual(read(t, fixture.candidate.EntryID), before) {
				t.Fatal("候选拒绝结果不符或发生写入", result, err)
			}
			checkLog(t, 0)
		})
	}

	t.Run("读回不一致时回滚", func(t *testing.T) {
		before := read(t, target.EntryID)
		execute(t, `CREATE TRIGGER fixture_change_expiry_purge_revision BEFORE UPDATE ON file_entries
			FOR EACH ROW SET NEW.revision = NEW.revision + 1`)
		t.Cleanup(func() { execute(t, "DROP TRIGGER fixture_change_expiry_purge_revision") })
		result, err := store.MarkExpiredTrashRootPurging(ctx, target)
		if result != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) || !reflect.DeepEqual(read(t, target.EntryID), before) {
			t.Fatal("异常读回未拒绝或事务未回滚", result, err)
		}
		checkLog(t, 0)
	})

	// 3. 在写入状态和序号后拒绝事件，验证两项写入均回滚。
	t.Run("事件插入失败时回滚", func(t *testing.T) {
		before := read(t, target.EntryID)
		const eventFailureMessage = "fixture rejected expiry event after marking"
		execute(t, `CREATE TRIGGER fixture_reject_expiry_event BEFORE INSERT ON change_events FOR EACH ROW
			BEGIN
				IF NEW.event_type='ENTRY_PURGE_STARTED' AND NEW.sequence=1
					AND EXISTS (SELECT 1 FROM file_entries WHERE id=13 AND id=NEW.entry_id AND user_id=NEW.user_id
						AND status='PURGING' AND revision=4 AND purge_started_at IS NOT NULL AND purged_at IS NULL)
					AND EXISTS (SELECT 1 FROM users WHERE id=NEW.user_id AND change_seq=1) THEN
					SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='`+eventFailureMessage+`';
				END IF;
			END`)
		t.Cleanup(func() { execute(t, "DROP TRIGGER fixture_reject_expiry_event") })
		result, err := store.MarkExpiredTrashRootPurging(ctx, target)
		var mysqlError *mysql.MySQLError
		if result != nil || !errors.As(err, &mysqlError) || mysqlError.Number != 1644 || mysqlError.Message != eventFailureMessage ||
			!reflect.DeepEqual(read(t, target.EntryID), before) {
			t.Fatal("事件插入失败后到期标记未完整回滚", result, err)
		}
		checkLog(t, 0)
		t.Log("MySQL 1644 拒绝事件后，TRASHED 状态、原版本、时间和账号序号均恢复")
	})

	// 4. 对同一候选并发重试，仅一次写入，随后重放不改变开始时间或日志。
	var beforeWrite, afterWrite time.Time
	if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&beforeWrite); err != nil {
		t.Fatal(err)
	}
	type attempt struct {
		root *domain.PurgeRootCandidate
		err  error
	}
	start := make(chan struct{})
	attempts := make(chan attempt, 2)
	for range 2 {
		go func() {
			<-start
			root, err := store.MarkExpiredTrashRootPurging(ctx, target)
			attempts <- attempt{root, err}
		}()
	}
	close(start)
	var marked *domain.PurgeRootCandidate
	staleCount := 0
	for range 2 {
		result := <-attempts
		switch {
		case result.err == nil && result.root != nil:
			marked = result.root
		case result.root == nil && errors.Is(result.err, domain.ErrExpiredTrashRootCandidateStale):
			staleCount++
		default:
			t.Error("并发标记返回意外结果", result.root, result.err)
		}
	}
	if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&afterWrite); err != nil {
		t.Fatal(err)
	}
	if marked == nil || staleCount != 1 || marked.EntryID != target.EntryID || marked.UserID != target.UserID ||
		marked.ParentID != target.ParentID || marked.Revision != target.Revision+1 ||
		marked.PurgeStartedAt.Before(beforeWrite) || marked.PurgeStartedAt.After(afterWrite) {
		t.Fatal("并发提交未产生唯一的清理根项", marked, staleCount)
	}
	committed := read(t, target.EntryID)
	if committed.Status != "PURGING" || committed.DeletedAt == nil || !committed.DeletedAt.Equal(old) ||
		committed.PurgeStartedAt == nil || !committed.PurgeStartedAt.Equal(marked.PurgeStartedAt) || committed.Revision != marked.Revision {
		t.Fatal("数据库未保存本轮开始状态", committed)
	}
	if result, err := store.MarkExpiredTrashRootPurging(ctx, target); result != nil || !errors.Is(err, domain.ErrExpiredTrashRootCandidateStale) ||
		!reflect.DeepEqual(read(t, target.EntryID), committed) {
		t.Fatal("重放旧候选改变了已提交记录", result, err)
	}
	checkLog(t, 1)
	var eventEntryID, revision uint64
	var eventType string
	var rawPayload []byte
	if err := pool.QueryRowContext(ctx, "SELECT entry_id,revision,event_type,payload FROM change_events WHERE user_id=1 AND `sequence`=1").
		Scan(&eventEntryID, &revision, &eventType, &rawPayload); err != nil {
		t.Fatal("未读取到期标记事件", err)
	}
	var payload map[string]string
	wantPayload := map[string]string{"entry_id": "13", "parent_id": "12", "name": "有效.md", "revision": "4"}
	if err := json.Unmarshal(rawPayload, &payload); err != nil || !reflect.DeepEqual(payload, wantPayload) ||
		eventEntryID != committed.ID || revision != committed.Revision || eventType != "ENTRY_PURGE_STARTED" {
		t.Fatal("自动标记事件与实际提交的条目或载荷契约不符", string(rawPayload), err)
	}
	roots, err := store.ListPurgeRootCandidates(ctx, 100)
	if err != nil || len(roots) != 1 || roots[0] != *marked {
		t.Fatal("现有扫描无法读到唯一的清理根项", roots, err)
	}
	var objectID, usedStorage uint64
	var purgedAt sql.NullTime
	err = pool.QueryRowContext(ctx, `SELECT entry.object_id, owner.used_storage, entry.purged_at
		FROM file_entries AS entry JOIN users AS owner ON owner.id=entry.user_id
		WHERE entry.user_id=1 AND entry.id=13`).Scan(&objectID, &usedStorage, &purgedAt)
	if err != nil || objectID != 41 || usedStorage != 4096 || purgedAt.Valid {
		t.Fatal("到期标记改变了对象引用、配额或完成时间", objectID, usedStorage, purgedAt, err)
	}
	t.Log("同一候选并发重试只写一次状态和序号 1，重放不刷新清理开始时间，原文件引用和配额保留")
}
