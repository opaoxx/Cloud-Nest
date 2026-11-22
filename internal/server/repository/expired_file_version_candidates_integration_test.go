package repository

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestListExpiredFileVersionCandidatesIntegration(t *testing.T) {
	// 1. 固定独占 MySQL 连接的时钟，精确控制微秒边界。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal("准备历史到期扫描夹具失败", err)
		}
	}
	now := time.Date(2026, 11, 23, 8, 0, 0, 0, time.UTC)
	execute("SET timestamp = ?", now.Unix())
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, err := pool.ExecContext(cleanup, "SET timestamp = DEFAULT"); err != nil {
			t.Error("恢复独占连接时钟失败", err)
		}
	})
	var databaseTime time.Time
	if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&databaseTime); err != nil || !databaseTime.Equal(now) {
		t.Fatal("测试连接未使用预期的 MySQL UTC 时间", databaseTime, err)
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	if candidates, err := store.ListExpiredFileVersionCandidates(ctx, 1); err != nil || candidates == nil || len(candidates) != 0 {
		t.Fatal("空库应交付空批次", candidates, err)
	}

	// 2. 准备两个账号的历史，覆盖当前文件已清理及旧内容为零字节的情况。
	execute(`INSERT INTO users(id,username,display_name,password_hash,storage_quota,used_storage,reserved_storage)
		VALUES(1,'history_expiry_owner','历史到期账号','fixture_hash',10000,714,11),
		(2,'history_expiry_other','零字节历史账号','fixture_hash',10000,0,17)`)
	execute(`INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size,status,verified_sha256,verified_at)
		VALUES(41,1,'local_minio','fixture-only','history-7b',7,'READY',UNHEX(REPEAT('ab',32)),UTC_TIMESTAMP(6)),
		(42,2,'local_minio','fixture-only','history-empty',0,'READY',UNHEX(REPEAT('cd',32)),UTC_TIMESTAMP(6))`)
	execute(`INSERT INTO file_entries(id,user_id,name,name_key,entry_type) VALUES(11,1,'',X'','DIR'),(21,2,'',X'','DIR')`)
	execute(`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,status,revision,object_id,file_size,purged_at)
		VALUES(12,1,11,'active.bin',X'61','FILE','OTHER','ACTIVE',200,41,7,NULL),
		(22,2,21,'purged.bin',X'62','FILE','OTHER','PURGED',4,NULL,0,UTC_TIMESTAMP(6))`)
	createdAt := now.AddDate(0, 0, -30)
	activeHistory := func(id, revision uint64, deadline time.Time) domain.FileVersion {
		return domain.FileVersion{ID: id, UserID: 1, EntryID: 12, Revision: revision, ObjectID: 41, FileSize: 7, CreatedAt: createdAt, RetainedUntil: deadline}
	}
	insertHistory := func(history domain.FileVersion) {
		t.Helper()
		execute(`INSERT INTO file_versions(id,user_id,entry_id,revision,object_id,file_size,created_at,retained_until)
			VALUES(?,?,?,?,?,?,?,?)`, history.ID, history.UserID, history.EntryID, history.Revision, history.ObjectID,
			history.FileSize, history.CreatedAt, history.RetainedUntil)
	}

	future := activeHistory(10, 1, now.Add(time.Microsecond))
	atDeadline := activeHistory(20, 2, now)
	purged := domain.FileVersion{ID: 30, UserID: 2, EntryID: 22, Revision: 1, ObjectID: 42, CreatedAt: createdAt, RetainedUntil: now}
	beforeDeadline := activeHistory(40, 3, now.Add(-time.Microsecond))
	for _, history := range []domain.FileVersion{purged, future, beforeDeadline, atDeadline} {
		insertHistory(history)
	}
	want := []domain.FileVersion{beforeDeadline, atDeadline, purged}
	for index := range domain.MaxExpiredFileVersionCandidates - 2 {
		history := activeHistory(uint64(100+index), uint64(4+index), now)
		insertHistory(history)
		want = append(want, history)
	}

	// 3. 核对边界、顺序、批量上限及扫描前后的历史、账目和对象计时。
	readHistory := func() []domain.FileVersion {
		t.Helper()
		var versions []domain.FileVersion
		if err := store.database.WithContext(ctx).Table("file_versions").Order("id ASC").Find(&versions).Error; err != nil {
			t.Fatal("读取隔离历史快照失败", err)
		}
		return versions
	}
	readAccounting := func() [2]string {
		t.Helper()
		var state [2]string
		for index := range state {
			if err := pool.QueryRowContext(ctx, `SELECT JSON_ARRAY(account.used_storage,account.reserved_storage,account.change_seq,
				account.updated_at,obj.status,obj.unreferenced_at,obj.gc_after,obj.updated_at)
				FROM users account JOIN stored_objects obj ON obj.owner_user_id=account.id WHERE account.id=?`, index+1).
				Scan(&state[index]); err != nil {
				t.Fatal("读取配额与对象计时失败", err)
			}
		}
		return state
	}
	historyBefore, accountingBefore := readHistory(), readAccounting()
	for _, limit := range []int{1, 2, 3, domain.MaxExpiredFileVersionCandidates, domain.MaxExpiredFileVersionCandidates} {
		candidates, err := store.ListExpiredFileVersionCandidates(ctx, limit)
		if err != nil || !slices.Equal(candidates, want[:limit]) {
			t.Fatalf("到期边界、排序或限量不符，要求 %d，得到 %d 条，错误 %v", limit, len(candidates), err)
		}
	}
	if !slices.Equal(readHistory(), historyBefore) || readAccounting() != accountingBefore {
		t.Fatal("只读扫描改变了历史记录、账目或对象回收计时")
	}
	for _, limit := range []int{0, domain.MaxExpiredFileVersionCandidates + 1} {
		if candidates, err := store.ListExpiredFileVersionCandidates(ctx, limit); candidates != nil || err == nil {
			t.Fatal("扫描接受了无效批量大小", limit, candidates, err)
		}
	}
	cancelled, stop := context.WithCancel(ctx)
	stop()
	if candidates, err := store.ListExpiredFileVersionCandidates(cancelled, 1); candidates != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("扫描未交付上下文取消原因", candidates, err)
	}
	if err := store.WithFileEntrySnapshot(ctx, func(txContext context.Context, txStore *Store) error {
		if candidates, err := txStore.ListExpiredFileVersionCandidates(txContext, 1); candidates != nil || err == nil {
			t.Error("事务中接受了跨账号扫描", candidates, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Logf("固定 MySQL UTC %s；截止点前 1 微秒和恰好截止入选，晚 1 微秒不入选；首三条 40、20、30，%d 条到期记录仅返回前 %d 条",
		now.Format(time.RFC3339Nano), len(want), domain.MaxExpiredFileVersionCandidates)
}
