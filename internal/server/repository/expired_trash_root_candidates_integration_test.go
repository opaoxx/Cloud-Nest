package repository

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestListExpiredTrashRootCandidatesIntegration(t *testing.T) {
	// 1. 固定独占 MySQL 连接的 UTC 时刻，准备两个账号的正式目录表。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal("准备到期回收站夹具失败", err)
		}
	}
	now := time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC)
	execute("SET timestamp = ?", now.Unix())
	t.Cleanup(func() {
		cleanupContext, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, err := pool.ExecContext(cleanupContext, "SET timestamp = DEFAULT"); err != nil {
			t.Error("恢复独占测试连接时钟失败", err)
		}
	})
	var databaseTime time.Time
	if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&databaseTime); err != nil || !databaseTime.Equal(now) {
		t.Fatal("独占连接的 MySQL 时刻不符", databaseTime, err)
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	if candidates, err := store.ListExpiredTrashRootCandidates(ctx, 100); err != nil || candidates == nil || len(candidates) != 0 {
		t.Fatal("空库应返回空候选批次", candidates, err)
	}
	execute(`INSERT INTO users(id,username,display_name,password_hash) VALUES
		(1,'expiry_owner','到期测试用户','fixture_hash'),(2,'expiry_other','另一测试用户','fixture_hash')`)
	execute(`INSERT INTO file_entries(id,user_id,name,name_key,entry_type) VALUES
		(11,1,'',X'','DIR'),(21,2,'',X'','DIR')`)
	execute(`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES
		(12,1,11,'活动目录',X'61','DIR'),(13,1,12,'活动后代',X'62','DIR')`)

	// 2. 混合严格边界、不同账号、文件与目录，以及藏在非活动祖先下的到期项。
	cutoff := now.AddDate(0, 0, -30)
	older, younger := cutoff.Add(-time.Microsecond), cutoff.Add(time.Microsecond)
	oldest := older.Add(-time.Microsecond)
	execute(`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at,revision) VALUES
		(22,2,21,'同刻外部账号',X'63','DIR','TRASHED',?,4),
		(24,1,13,'活动目录下的候选',X'64','DIR','TRASHED',?,5),
		(25,1,11,'恰好三十天',X'65','DIR','TRASHED',?,2),
		(26,1,11,'差一微秒',X'66','DIR','TRASHED',?,2),
		(30,1,11,'未到期回收祖先',X'67','DIR','TRASHED',?,2)`, older, oldest, cutoff, younger, younger)
	execute(`INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size)
		VALUES(41,1,'local_minio','fixture-only','expired-trash-fixture',3)`)
	execute(`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at,revision,category,object_id,file_size)
		VALUES(23,1,11,'同刻文件',X'68','FILE','TRASHED',?,3,'OTHER',41,3)`, older)
	execute(`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at,purge_started_at) VALUES
		(34,1,11,'清理中祖先',X'69','DIR','PURGING',?,?)`, oldest, oldest)
	execute(`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES
		(32,1,30,'隐藏活动后代',X'6a','DIR'),(35,1,34,'清理中活动后代',X'6b','DIR')`)
	execute(`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at) VALUES
		(31,1,30,'回收目录直接后代',X'6c','DIR','TRASHED',?),
		(33,1,32,'回收目录间接后代',X'6d','DIR','TRASHED',?),
		(36,1,35,'清理目录间接后代',X'6e','DIR','TRASHED',?)`, oldest, oldest, oldest)

	parentID := 11
	for depth := 1; depth <= domain.MaxFileEntryDepth; depth++ {
		entryID := 100 + depth
		execute(`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type)
			VALUES(?,1,?,'深层活动目录',X'6f','DIR')`, entryID, parentID)
		parentID = entryID
	}
	execute(`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at) VALUES
		(180,1,?,'最大合法深度',X'70','DIR','TRASHED',?),
		(181,1,?,'超过深度限制',X'71','DIR','TRASHED',?)`, parentID-1, oldest, parentID, oldest)

	// 3. 核对顺序、限量和前后快照，确认候选筛选不会写入条目。
	type entryState struct {
		ID, Revision                                   uint64
		ParentID                                       *uint64
		Status                                         string
		DeletedAt, PurgeStartedAt, PurgedAt, UpdatedAt *time.Time
	}
	readStates := func() []entryState {
		t.Helper()
		var states []entryState
		if err := store.database.WithContext(ctx).Table("file_entries").
			Select("id", "revision", "parent_id", "status", "deleted_at", "purge_started_at", "purged_at", "updated_at").
			Order("id ASC").Find(&states).Error; err != nil {
			t.Fatal("读取夹具状态失败", err)
		}
		return states
	}
	before := readStates()
	want := []domain.ExpiredTrashRootCandidate{
		{EntryID: 24, UserID: 1, ParentID: 13, Revision: 5, DeletedAt: oldest},
		{EntryID: 180, UserID: 1, ParentID: uint64(parentID - 1), Revision: 1, DeletedAt: oldest},
		{EntryID: 22, UserID: 2, ParentID: 21, Revision: 4, DeletedAt: older},
		{EntryID: 23, UserID: 1, ParentID: 11, Revision: 3, DeletedAt: older},
	}
	for _, limit := range []int{1, 3, 100, 100} {
		if candidates, err := store.ListExpiredTrashRootCandidates(ctx, limit); err != nil ||
			!reflect.DeepEqual(candidates, want[:min(limit, len(want))]) {
			t.Fatal("到期候选边界、根项、顺序或范围不符", limit, candidates, err)
		}
	}
	if after := readStates(); !reflect.DeepEqual(after, before) {
		t.Fatal("只读扫描改变了目录项状态或时间")
	}
	for _, limit := range []int{0, 101} {
		if candidates, err := store.ListExpiredTrashRootCandidates(ctx, limit); candidates != nil || err == nil {
			t.Fatal("接受了无效扫描上限", limit, candidates, err)
		}
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if candidates, err := store.ListExpiredTrashRootCandidates(canceled, 1); candidates != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("未返回上下文取消原因", candidates, err)
	}
	if err := store.WithFileEntrySnapshot(ctx, func(readContext context.Context, reader *Store) error {
		if candidates, err := reader.ListExpiredTrashRootCandidates(readContext, 1); candidates != nil || err == nil {
			t.Error("允许在用户目录快照中执行跨账号扫描", candidates, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
