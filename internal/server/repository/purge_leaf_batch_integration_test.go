package repository

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestCompletePurgeLeafBatchIntegration(t *testing.T) {
	// 1. 在同一个清理根目录下准备两个文件和一个空目录。
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	pool, store := purgeLeafSettlementTestDatabase(t, ctx)
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal("准备批次结算夹具失败", err)
		}
	}
	execute("UPDATE file_entries SET status='PURGING',purge_started_at=UTC_TIMESTAMP(6),revision=revision+1 WHERE id=14")
	execute(`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size)
		VALUES (17,1,14,'待清理.md',CAST('待清理.md' AS BINARY),'FILE','DOCUMENT',41,2048)`)
	execute(`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,revision)
		VALUES (18,1,14,'空目录',CAST('空目录' AS BINARY),'DIR',?)`, uint64(math.MaxUint64))
	execute(`INSERT INTO user_entry_preferences(user_id,entry_id,starred_at)
		SELECT user_id,id,UTC_TIMESTAMP(6) FROM file_entries WHERE user_id=1 AND id IN (13,14,15,17,18)`)
	execute("UPDATE users SET storage_quota=10000,used_storage=2048,reserved_storage=512 WHERE id=1")

	roots, err := store.ListPurgeRootCandidates(ctx, 1)
	if err != nil || len(roots) != 1 || roots[0].EntryID != 14 {
		t.Fatal("未取得批次夹具的清理根项", roots, err)
	}
	root := roots[0]
	assertEntry := func(entryID uint64, wantStatus string, wantRevision, wantObjectID uint64) {
		t.Helper()
		var status string
		var revision, objectID uint64
		var hasCompletionTime, hasPreference bool
		err := pool.QueryRowContext(ctx, `
			SELECT entry.status,entry.revision,COALESCE(entry.object_id,0),entry.purged_at IS NOT NULL,
			       EXISTS(SELECT 1 FROM user_entry_preferences WHERE user_id=entry.user_id AND entry_id=entry.id)
			FROM file_entries AS entry WHERE entry.user_id=1 AND entry.id=?`, entryID).
			Scan(&status, &revision, &objectID, &hasCompletionTime, &hasPreference)
		wantCompleted := wantStatus == "PURGED"
		if err != nil || status != wantStatus || revision != wantRevision || objectID != wantObjectID ||
			hasCompletionTime != wantCompleted || hasPreference != !wantCompleted {
			t.Fatalf("目录项 %d 实际状态 %s，版本 %d，引用 %d，完成时间存在 %t，偏好存在 %t，错误 %v",
				entryID, status, revision, objectID, hasCompletionTime, hasPreference, err)
		}
	}
	assertQuota := func(wantUsed uint64) {
		t.Helper()
		var used, reserved uint64
		err := pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage FROM users WHERE id=1").Scan(&used, &reserved)
		if err != nil || used != wantUsed || reserved != 512 {
			t.Fatalf("已用空间为 %d，期望 %d，预占空间为 %d，错误 %v", used, wantUsed, reserved, err)
		}
	}
	assertEventCount := func(want uint64) {
		t.Helper()
		var sequence, events uint64
		if err := pool.QueryRowContext(ctx, `SELECT change_seq,
			(SELECT COUNT(*) FROM change_events WHERE user_id=users.id) FROM users WHERE id=1`).
			Scan(&sequence, &events); err != nil || sequence != want || events != want {
			t.Fatal("清理完成事件数或账号序号不符", events, sequence, err)
		}
	}

	// 2. 拒绝越界的批量大小，模拟第二个文件配额不足，回查整批均未提交。
	for _, limit := range []int{0, domain.MaxPurgeLeafCandidates + 1} {
		if count, err := store.CompletePurgeLeafBatch(ctx, root, limit); err == nil || count != 0 {
			t.Fatal("接受了越界的批量大小", limit, count, err)
		}
	}
	count, err := store.CompletePurgeLeafBatch(ctx, root, 2)
	if count != 0 || !errors.Is(err, domain.ErrInvalidAccountData) || !strings.Contains(err.Error(), "叶子 17 失败") {
		t.Fatal("第二个文件配额不足时应返回零和原始错误", count, err)
	}
	assertQuota(2048)
	assertEntry(14, "PURGING", root.Revision, 0)
	assertEntry(15, "ACTIVE", 5, 41)
	assertEntry(17, "ACTIVE", 1, 41)
	assertEntry(18, "ACTIVE", math.MaxUint64, 0)
	assertEventCount(0)

	// 3. 账目恢复后，让第二条事件写入失败，确认第一条也随整批回滚。
	execute("UPDATE users SET used_storage=6144 WHERE id=1")
	const eventFailureMessage = "fixture rejected second purge event after settlement"
	execute(`CREATE TRIGGER fixture_reject_second_purge_event BEFORE INSERT ON change_events FOR EACH ROW
		BEGIN
			IF NEW.user_id=1 AND NEW.entry_id=17 AND NEW.sequence=2 AND NEW.event_type='ENTRY_PURGED'
				AND EXISTS (SELECT 1 FROM change_events AS events WHERE user_id=1 AND events.sequence=1 AND entry_id=15)
				AND (SELECT COUNT(*) FROM file_entries WHERE id IN (15,17) AND status='PURGED' AND object_id IS NULL AND purged_at IS NOT NULL)=2
				AND NOT EXISTS (SELECT 1 FROM user_entry_preferences WHERE user_id=1 AND entry_id IN (15,17))
				AND EXISTS (SELECT 1 FROM users WHERE id=1 AND change_seq=2 AND used_storage=2048 AND reserved_storage=512) THEN
				SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='` + eventFailureMessage + `';
			END IF;
		END`)
	count, err = store.CompletePurgeLeafBatch(ctx, root, 2)
	var databaseError *mysql.MySQLError
	if count != 0 || !errors.As(err, &databaseError) || databaseError.Number != 1644 || databaseError.Message != eventFailureMessage {
		t.Fatal("第二项应在状态、配额和首项事件都已写入后失败", count, err)
	}
	execute("DROP TRIGGER fixture_reject_second_purge_event")
	assertQuota(6144)
	assertEntry(15, "ACTIVE", 5, 41)
	assertEntry(17, "ACTIVE", 1, 41)
	assertEventCount(0)
	t.Log("第二条事件被拒绝后，整批文件、对象引用、偏好、配额和首条事件均回滚")

	// 4. 重试，先按上限结算一个文件，再结算另外两个叶子。
	if count, err := store.CompletePurgeLeafBatch(ctx, root, 1); err != nil || count != 1 {
		t.Fatal("限制为一项时应只提交一个叶子", count, err)
	}
	assertQuota(4096)
	assertEntry(15, "PURGED", 6, 0)
	assertEntry(17, "ACTIVE", 1, 41)
	assertEntry(18, "ACTIVE", math.MaxUint64, 0)
	assertEventCount(1)

	if count, err := store.CompletePurgeLeafBatch(ctx, root, domain.MaxPurgeLeafCandidates); err != nil || count != 2 {
		t.Fatal("第二批应提交剩余的两个叶子", count, err)
	}
	assertQuota(2048)
	assertEntry(17, "PURGED", 2, 0)
	assertEntry(18, "PURGED", math.MaxUint64, 0)
	assertEntry(14, "PURGING", root.Revision, 0)
	assertEventCount(3)

	// 5. 后代完成后的下一批才结算根项，旧根项不再结算或追加事件。
	if count, err := store.CompletePurgeLeafBatch(ctx, root, domain.MaxPurgeLeafCandidates); err != nil || count != 1 {
		t.Fatal("最后一批应仅完成根项", count, err)
	}
	assertEntry(14, "PURGED", root.Revision+1, 0)
	var startedAt time.Time
	err = pool.QueryRowContext(ctx, "SELECT purge_started_at FROM file_entries WHERE id=14").Scan(&startedAt)
	if err != nil || !startedAt.Equal(root.PurgeStartedAt) {
		t.Fatal("根项原清理开始时间应保留", startedAt, err)
	}
	if count, err := store.CompletePurgeLeafBatch(ctx, root, 1); count != 0 || !errors.Is(err, domain.ErrPurgeRootCandidateStale) {
		t.Fatal("已完成的根项应拒绝重复结算", count, err)
	}
	assertQuota(2048)
	assertEntry(13, "ACTIVE", 3, 41)
	assertEventCount(4)
	for index, want := range []struct {
		entryID, parentID, revision uint64
		name                        string
	}{
		{15, 14, 6, "隐藏.md"},
		{17, 14, 2, "待清理.md"},
		{18, 14, math.MaxUint64, "空目录"},
		{14, 11, root.Revision + 1, "已回收目录"},
	} {
		var entryID, revision uint64
		var eventType string
		var rawPayload []byte
		if err := pool.QueryRowContext(ctx, "SELECT entry_id,revision,event_type,payload FROM change_events WHERE user_id=1 AND `sequence`=?",
			index+1).Scan(&entryID, &revision, &eventType, &rawPayload); err != nil {
			t.Fatal("读取清理完成事件失败", index+1, err)
		}
		expectedPayload := map[string]string{
			"entry_id": strconv.FormatUint(want.entryID, 10), "parent_id": strconv.FormatUint(want.parentID, 10),
			"name": want.name, "revision": strconv.FormatUint(want.revision, 10),
		}
		var payload map[string]string
		if err := json.Unmarshal(rawPayload, &payload); err != nil || !reflect.DeepEqual(payload, expectedPayload) ||
			entryID != want.entryID || revision != want.revision || eventType != "ENTRY_PURGED" {
			t.Fatal("完成事件的顺序或墓碑不符", index+1, string(rawPayload), err)
		}
	}
	t.Log("文件 15、17、空目录 18、根项 14 依次提交为序号 1–4，目录 18 的最高版本原样写入墓碑")
}
