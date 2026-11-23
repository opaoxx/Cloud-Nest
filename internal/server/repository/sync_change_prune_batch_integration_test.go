package repository

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestSyncChangePruneBatchIntegration(t *testing.T) {
	// 1. 在独占库准备无候选页、实际可裁剪账号和设备尚未确认的账号。
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, store, _ := trashEntryTestDatabase(t, ctx)
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
	execute(`INSERT INTO users(id,username,display_name,password_hash,change_seq) VALUES
		(3,'fixture_failure','失败','fixture',19),(4,'fixture_prunable','可裁剪','fixture',19),
		(5,'fixture_blocked','受保护','fixture',19),(6,'fixture_boundary','临界','fixture',19),
		(7,'fixture_later','下轮','fixture',19)`)
	execute("INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(59,5,'28ee758a-0727-42de-ae7d-3bb33155e159','Fixture','WEB')")
	reference := time.Date(2026, time.November, 23, 12, 0, 0, 15625000, time.UTC)
	cutoff := reference.AddDate(0, 0, -90)
	older := cutoff.Add(-time.Microsecond)
	const insertEvent = "INSERT INTO change_events(user_id,`sequence`,entry_id,revision,event_type,payload,created_at) " +
		"VALUES(?,?,11,1,'FIXTURE_CHANGE','{}',?)"
	for userID := uint64(3); userID <= 7; userID++ {
		head := older
		if userID == 6 {
			head = cutoff
		}
		execute(insertEvent, userID, 18, head)
		execute(insertEvent, userID, 19, older)
	}
	execute(`INSERT INTO sync_devices(device_id,user_id,status,ack_sequence,cursor_generation,last_seen_at)
		VALUES(59,5,'ACTIVE',17,UNHEX(REPEAT('11',16)),?)`, reference)
	execute("SET timestamp = ?", float64(reference.Unix())+0.015625)
	defer execute("SET timestamp = DEFAULT")

	// 2. 使用真实仓储运行两轮。首页后推进数据库时钟，首轮仍使用原截止点。
	type pageCall struct {
		after  uint64
		limit  int
		cutoff time.Time
	}
	var pages []pageCall
	var attempted []uint64
	reads := 0
	failure := errors.New("账号 3 的合成事务故障")
	batch := service.NewSyncChangePruneBatch(func(queryContext context.Context) (time.Time, error) {
		reads++
		return store.ReadSyncChangePruneCutoff(queryContext)
	}, func(queryContext context.Context, stamp time.Time, after uint64, limit int) (domain.SyncChangePruneCandidatePage, error) {
		pages = append(pages, pageCall{after, limit, stamp})
		page, err := store.ListSyncChangePruneCandidates(queryContext, stamp, after, limit)
		if err == nil && len(pages) == 1 {
			if page.ScannedCount != 2 || len(page.UserIDs) != 0 {
				t.Fatalf("首个扫描页应无候选：%+v", page)
			}
			execute("SET timestamp = ?", float64(reference.Unix())+1.015625)
		}
		return page, err
	}, func(txContext context.Context, userID uint64, stamp time.Time, limit int) (domain.SyncChangePruneRange, error) {
		attempted = append(attempted, userID)
		expected := cutoff.Add(time.Duration(reads-1) * time.Second)
		if stamp != expected || limit != 1 {
			t.Fatalf("裁剪收到错误截止点或上限：%v，%d", stamp, limit)
		}
		if userID == 3 {
			return domain.SyncChangePruneRange{}, failure
		}
		return store.PruneSyncChangeLogBatch(txContext, userID, stamp, limit)
	})
	limits := service.SyncChangePruneBatchLimits{ScanLimit: 5, PageSize: 2, AccountDeleteLimit: 1}
	first, err := batch.RunOnce(ctx, 0, limits)
	if err != nil || first.Cutoff != cutoff || first.Scanned != 5 || first.PrunedAccounts != 1 ||
		first.DeletedEvents != 1 || first.EmptyAccounts != 1 || first.NextAfterUserID != 5 ||
		len(first.AccountErrors) != 1 || first.AccountErrors[0].UserID != 3 || !errors.Is(first.AccountErrors[0].Err, failure) {
		t.Fatalf("首轮结果不符：%+v，错误 %v", first, err)
	}
	if reads != 1 || !slices.Equal(pages, []pageCall{{0, 2, cutoff}, {2, 2, cutoff}, {4, 1, cutoff}}) ||
		!slices.Equal(attempted, []uint64{3, 4, 5}) {
		t.Fatalf("首轮重复读钟或分页、裁剪调用不符：%d，%+v，%v", reads, pages, attempted)
	}
	second, err := batch.RunOnce(ctx, first.NextAfterUserID, limits)
	later := cutoff.Add(time.Second)
	if err != nil || second.Cutoff != later || second.Scanned != 2 || second.PrunedAccounts != 2 ||
		second.DeletedEvents != 2 || second.EmptyAccounts != 0 || second.NextAfterUserID != 0 || len(second.AccountErrors) != 0 ||
		reads != 2 || !slices.Equal(pages[3:], []pageCall{{5, 2, later}, {7, 2, later}}) || !slices.Equal(attempted, []uint64{3, 4, 5, 6, 7}) {
		t.Fatalf("第二轮未正确续扫或结束：%+v，错误 %v，读取 %d，分页 %+v，尝试 %v", second, err, reads, pages, attempted)
	}

	// 3. 从数据库确认每个账号只移除了允许提交的范围，最大变更序号继续保留。
	for _, fixture := range []struct {
		userID uint64
		count  int
		first  uint64
	}{{3, 2, 18}, {4, 1, 19}, {5, 2, 18}, {6, 1, 19}, {7, 1, 19}} {
		var high, firstSequence uint64
		var count int
		err := pool.QueryRowContext(ctx, "SELECT account.change_seq, COUNT(event.`sequence`), MIN(event.`sequence`) "+
			"FROM users AS account JOIN change_events AS event ON event.user_id=account.id WHERE account.id=? GROUP BY account.change_seq", fixture.userID).
			Scan(&high, &count, &firstSequence)
		if err != nil || high != 19 || count != fixture.count || firstSequence != fixture.first {
			t.Fatalf("账号 %d 的已提交状态不符：最大序号 %d，首条 %d，条数 %d，错误 %v",
				fixture.userID, high, firstSequence, count, err)
		}
	}
}
