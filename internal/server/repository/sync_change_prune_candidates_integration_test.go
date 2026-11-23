package repository

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestListSyncChangePruneCandidatesIntegration(t *testing.T) {
	// 1. 用多个合成账号建立有候选、无候选、空日志和空页。
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	installChangeEventFixtureTable(t, ctx, pool)
	_, deviceDDL, found := strings.Cut(migrations.Ordered()[2].SQL, ";\n")
	if !found {
		t.Fatal("未找到正式同步设备表")
	}
	execute(deviceDDL)
	const largeUserID = uint64(math.MaxUint64 - 1)
	execute(`INSERT INTO users(id,username,display_name,password_hash) VALUES
		(3,'fixture_exact','恰好截止点','fixture'),
		(4,'fixture_empty','空日志','fixture'),
		(5,'fixture_newer','截止点之后','fixture'),
		(6,'fixture_old','截止点之前','fixture'),
		(?,'fixture_large','无符号账号','fixture')`, largeUserID)
	execute("UPDATE users SET change_seq=19 WHERE id<>4")
	execute("UPDATE users SET status='DISABLED' WHERE id=2")

	cutoff := time.Date(2026, time.August, 25, 12, 0, 0, 123456000, time.UTC)
	older := cutoff.Add(-time.Microsecond)
	newer := cutoff.Add(time.Microsecond)
	const insertEvent = "INSERT INTO change_events(user_id,`sequence`,entry_id,revision,event_type,payload,created_at) " +
		"VALUES(?,?,11,1,'FIXTURE_CHANGE','{}',?)"
	for _, fixture := range []struct {
		userID     uint64
		firstTime  time.Time
		secondTime time.Time
	}{
		{1, older, newer},
		{2, older, older},
		{3, cutoff, older},
		{5, newer, older},
		{6, older, newer},
		{largeUserID, older, older},
	} {
		execute(insertEvent, fixture.userID, 18, fixture.firstTime)
		execute(insertEvent, fixture.userID, 19, fixture.secondTime)
	}
	execute(`INSERT INTO sync_devices(device_id,user_id,status,ack_sequence,cursor_generation,last_seen_at)
		VALUES(?,?,'ACTIVE',17,UNHEX(REPEAT('11',16)),?)`, actor.DeviceID, actor.UserID, cutoff)

	assertPage := func(deadline time.Time, after uint64, limit, scanned int, next uint64, want ...uint64) {
		t.Helper()
		page, err := store.ListSyncChangePruneCandidates(ctx, deadline, after, limit)
		if err != nil || page.ScannedCount != scanned || page.NextAfterUserID != next || !slices.Equal(page.UserIDs, want) {
			t.Fatalf("从 %d 开始扫描得到 %+v，错误 %v，预期检查 %d 个、下一位置 %d、候选 %v", after, page, err, scanned, next, want)
		}
	}

	// 2. 空候选页仍要推进。首条的序号决定检查顺序，后续更早的时间不能绕开首条。
	assertPage(cutoff, 0, 2, 2, 2, 1, 2)
	assertPage(cutoff, 2, 2, 2, 4)
	assertPage(cutoff, 4, 2, 2, 6, 6)
	assertPage(cutoff, 6, 2, 1, largeUserID, largeUserID)
	assertPage(cutoff, largeUserID, 2, 0, largeUserID)
	assertPage(cutoff, math.MaxUint64, 2, 0, math.MaxUint64)
	assertPage(cutoff.In(time.FixedZone("fixture", 8*60*60)), 0, 100, 7, largeUserID, 1, 2, 6, largeUserID)

	// 3. 确认位置仍由单账号事务判断。拒绝事务内扫描、无效输入和取消。
	assertRejected := func(queryContext context.Context, deadline time.Time, limit int, wantErr error) {
		t.Helper()
		page, err := store.ListSyncChangePruneCandidates(queryContext, deadline, 2, limit)
		if page.ScannedCount != 0 || page.NextAfterUserID != 0 || len(page.UserIDs) != 0 || err == nil ||
			(wantErr != nil && !errors.Is(err, wantErr)) {
			t.Fatalf("无效扫描仍返回结果：%+v，错误 %v，期望错误 %v", page, err, wantErr)
		}
	}
	assertRejected(ctx, cutoff, 0, nil)
	assertRejected(ctx, cutoff, domain.MaxSyncChangePruneAccountPageSize+1, nil)
	assertRejected(ctx, time.Time{}, 2, nil)
	assertRejected(ctx, cutoff.Add(time.Nanosecond), 2, nil)
	cancelled, stop := context.WithCancel(ctx)
	stop()
	assertRejected(cancelled, cutoff, 2, context.Canceled)

	if err := store.withLockedUserTransaction(ctx, actor.UserID, true, func(txContext context.Context, txStore *Store) error {
		prefix, err := txStore.selectSyncChangePrunePrefix(txContext, cutoff, 2)
		if err != nil || prefix != (domain.SyncChangePruneRange{}) {
			return fmt.Errorf("首个候选账号应受设备位置保护：%+v，%v", prefix, err)
		}
		page, err := txStore.ListSyncChangePruneCandidates(txContext, cutoff, 0, 2)
		if err == nil || page.ScannedCount != 0 || len(page.UserIDs) != 0 {
			return fmt.Errorf("事务内扫描未被拒绝：%+v，%v", page, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	var eventCount, ackPosition uint64
	err := pool.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM change_events),
		(SELECT ack_sequence FROM sync_devices WHERE device_id=?)`, actor.DeviceID).Scan(&eventCount, &ackPosition)
	if err != nil || eventCount != 12 || ackPosition != 17 {
		t.Fatalf("扫描后日志或设备位置不符：%d，%d，%v", eventCount, ackPosition, err)
	}
}
