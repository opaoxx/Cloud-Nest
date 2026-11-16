package repository

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestListUserGlobalEntryCandidatesIntegration(t *testing.T) {
	// 1. 使用正式表独占库准备不同用户、父目录、状态和同时间记录。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(2)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash) VALUES(1,'fixture_one','全局候选用户','fixture_hash'),(3,'fixture_three','其他用户','fixture_hash'),(5,'fixture_empty','空用户','fixture_hash')",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','fixture-global-object',2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(33,3,NULL,'',X'','DIR'),(55,5,NULL,'',X'','DIR'),(12,1,11,'课程目录',CAST('课程目录' AS BINARY),'DIR'),(14,1,11,'其他目录',CAST('其他目录' AS BINARY),'DIR')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at) VALUES(16,1,11,'回收目录',CAST('回收目录' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6)),(106,1,12,'回收子项',CAST('回收子项' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,revision) VALUES(101,1,12,'学习笔记 🌱.md',CAST('学习笔记 🌱.md' AS BINARY),'FILE','DOCUMENT',41,2048,3),(105,1,14,'旧笔记.md',CAST('旧笔记.md' AS BINARY),'FILE','DOCUMENT',41,2048,1)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(102,1,12,'资料二',CAST('资料二' AS BINARY),'DIR'),(103,1,14,'资料三',CAST('资料三' AS BINARY),'DIR'),(104,1,12,'资料四',CAST('资料四' AS BINARY),'DIR'),(110,3,33,'其他用户子目录',CAST('其他用户子目录' AS BINARY),'DIR'),(111,1,16,'回收目录中的活动项',CAST('回收目录中的活动项' AS BINARY),'DIR')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,purge_started_at) VALUES(107,1,12,'清理中',CAST('清理中' AS BINARY),'DIR','PURGING',UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,file_size,status,purged_at) VALUES(108,1,12,'已清理.bin',CAST('已清理.bin' AS BINARY),'FILE','OTHER',64,'PURGED',UTC_TIMESTAMP(6))",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("建立全局候选夹具失败", err)
		}
	}
	stamp := time.Date(2026, 11, 15, 2, 30, 0, 123456000, time.UTC)
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET created_at=?,modified_at=?,updated_at=?", stamp.Add(-time.Hour), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET modified_at=? WHERE id IN (101,102)", stamp.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET modified_at=? WHERE id IN (103,104)", stamp.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	checkIDs := func(t *testing.T, page *domain.FileEntryPage, expected []uint64, more bool) {
		t.Helper()
		if page == nil || page.Entries == nil || len(page.Entries) != len(expected) || page.HasMore != more {
			t.Fatal("候选页长度、空切片或后续页提示错误")
		}
		for index, entry := range page.Entries {
			if entry.ID != expected[index] || entry.Status != "ACTIVE" || entry.ParentID == nil {
				t.Fatal("候选范围或顺序错误")
			}
		}
	}

	// 2. 验证全局复合位置翻页、完整字段、范围和独立结果。
	t.Run("stable pagination across parents and complete fields", func(t *testing.T) {
		var before *domain.FileEntryPosition
		var allIDs []uint64
		expectedPages := [][]uint64{{102, 101}, {104, 103}, {111, 105}, {14, 12}}
		for index, expected := range expectedPages {
			page, err := store.ListUserGlobalEntryCandidates(ctx, 1, globalEntryCandidateQuery(2), before)
			if err != nil {
				t.Fatal(err)
			}
			checkIDs(t, page, expected, index < len(expectedPages)-1)
			for _, entry := range page.Entries {
				stored, err := store.FindUserFileEntry(ctx, 1, entry.ID)
				if err != nil || stored == nil || !reflect.DeepEqual(entry, *stored) {
					t.Fatal("全局候选十一列与数据库事实不一致", err)
				}
				allIDs = append(allIDs, entry.ID)
			}
			last := page.Entries[len(page.Entries)-1]
			before = &domain.FileEntryPosition{ModifiedAt: last.ModifiedAt.In(time.FixedZone("fixture", 8*3600)), ID: last.ID}
			savedPosition := *before
			if _, err := store.ListUserGlobalEntryCandidates(ctx, 1, globalEntryCandidateQuery(2), before); err != nil || *before != savedPosition {
				t.Fatal("时区位置翻页失败或输入被修改", err)
			}
		}
		if !reflect.DeepEqual(allIDs, []uint64{102, 101, 104, 103, 111, 105, 14, 12}) {
			t.Fatal("全局翻页漏项或重复")
		}
		empty, err := store.ListUserGlobalEntryCandidates(ctx, 1, globalEntryCandidateQuery(2), before)
		if err != nil {
			t.Fatal(err)
		}
		checkIDs(t, empty, nil, false)
	})
	t.Run("scope lifecycle and root exclusion", func(t *testing.T) {
		for _, fixture := range []struct {
			userID uint64
			ids    []uint64
		}{
			{userID: 1, ids: []uint64{102, 101, 104, 103, 111, 105, 14, 12}},
			{userID: 3, ids: []uint64{110}},
			{userID: 5},
			{userID: 999},
		} {
			page, err := store.ListUserGlobalEntryCandidates(ctx, fixture.userID, globalEntryCandidateQuery(100), nil)
			if err != nil {
				t.Fatal(err)
			}
			checkIDs(t, page, fixture.ids, false)
		}
	})
	t.Run("independent copies and read only facts", func(t *testing.T) {
		first, err := store.ListUserGlobalEntryCandidates(ctx, 1, globalEntryCandidateQuery(100), nil)
		if err != nil {
			t.Fatal(err)
		}
		first.Entries[0].Name = "本地副本"
		*first.Entries[0].ParentID = 999
		if *first.Entries[1].ParentID != 12 {
			t.Fatal("不同候选共享父目录指针")
		}
		again, err := store.ListUserGlobalEntryCandidates(ctx, 1, globalEntryCandidateQuery(100), nil)
		if err != nil || again.Entries[0].Name != "资料二" || *again.Entries[0].ParentID != 12 {
			t.Fatal("本地副本影响了后续读取", err)
		}
	})

	// 3. 验证百条页、未提交写入及快照一致性。
	t.Run("maximum page size", func(t *testing.T) {
		for index := 0; index < 101; index++ {
			name := fmt.Sprintf("bulk-%d", index)
			if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,modified_at) VALUES(?,5,55,?,?,'DIR',?)", 1000+index, name, []byte(name), stamp); err != nil {
				t.Fatal(err)
			}
		}
		first, err := store.ListUserGlobalEntryCandidates(ctx, 5, globalEntryCandidateQuery(100), nil)
		if err != nil || first == nil || len(first.Entries) != 100 || !first.HasMore || first.Entries[0].ID != 1100 || first.Entries[99].ID != 1001 {
			t.Fatal("百条候选页及额外记录错误", err)
		}
		last := first.Entries[99]
		second, err := store.ListUserGlobalEntryCandidates(ctx, 5, globalEntryCandidateQuery(100), &domain.FileEntryPosition{ModifiedAt: last.ModifiedAt, ID: last.ID})
		if err != nil {
			t.Fatal(err)
		}
		checkIDs(t, second, []uint64{1000}, false)
	})
	t.Run("uncommitted write preserves committed page", func(t *testing.T) {
		before, err := store.ListUserGlobalEntryCandidates(ctx, 1, globalEntryCandidateQuery(100), nil)
		if err != nil {
			t.Fatal(err)
		}
		writer, err := pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer writer.Rollback()
		if _, err := writer.ExecContext(ctx, "UPDATE file_entries SET name='未提交名称',modified_at=UTC_TIMESTAMP(6) WHERE id=104"); err != nil {
			t.Fatal(err)
		}
		readContext, release := context.WithTimeout(ctx, 500*time.Millisecond)
		defer release()
		current, err := store.ListUserGlobalEntryCandidates(readContext, 1, globalEntryCandidateQuery(100), nil)
		if err != nil || !reflect.DeepEqual(before, current) {
			t.Fatal("未提交写入影响候选页或等待了行锁", err)
		}
	})
	t.Run("snapshot retains candidates across insert and trash", func(t *testing.T) {
		if err := store.WithFileEntrySnapshot(ctx, func(txContext context.Context, txStore *Store) error {
			first, err := txStore.ListUserGlobalEntryCandidates(txContext, 1, globalEntryCandidateQuery(2), nil)
			if err != nil {
				return err
			}
			checkIDs(t, first, []uint64{102, 101}, true)
			writer, err := pool.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer writer.Rollback()
			if _, err := writer.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,modified_at) VALUES(112,1,14,'新候选',CAST('新候选' AS BINARY),'DIR',?)", stamp.Add(9*time.Second)); err != nil {
				return err
			}
			if _, err := writer.ExecContext(ctx, "UPDATE file_entries SET status='TRASHED',deleted_at=UTC_TIMESTAMP(6),modified_at=? WHERE id=102", stamp.Add(8*time.Second)); err != nil {
				return err
			}
			if err := writer.Commit(); err != nil {
				return err
			}
			again, err := txStore.ListUserGlobalEntryCandidates(txContext, 1, globalEntryCandidateQuery(2), nil)
			if err != nil || !reflect.DeepEqual(first, again) {
				t.Fatal("候选快照混入后续写入", err)
			}
			last := first.Entries[1]
			next, err := txStore.ListUserGlobalEntryCandidates(txContext, 1, globalEntryCandidateQuery(2), &domain.FileEntryPosition{ModifiedAt: last.ModifiedAt, ID: last.ID})
			if err != nil {
				return err
			}
			checkIDs(t, next, []uint64{104, 103}, true)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		current, err := store.ListUserGlobalEntryCandidates(ctx, 1, globalEntryCandidateQuery(2), nil)
		if err != nil {
			t.Fatal(err)
		}
		checkIDs(t, current, []uint64{112, 101}, true)
	})
	// 4. 最后建立最大主键夹具，核对完整无符号整数及 MySQL 时间两端。
	t.Run("full unsigned values and datetime boundaries", func(t *testing.T) {
		maximum := ^uint64(0)
		if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash) VALUES(?,'fixture_max','最大整数用户','fixture_hash')", maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(?,?,'MINIO','fixture-bucket','fixture-max-object',?)", maximum, maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,?,NULL,'',X'','DIR')", maximum-3, maximum); err != nil {
			t.Fatal(err)
		}
		minimumTime := time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)
		maximumTime := time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
		for _, entryID := range []uint64{maximum - 2, maximum - 1} {
			modifiedAt := maximumTime
			if entryID == maximum-2 {
				modifiedAt = minimumTime
			}
			name := fmt.Sprintf("max-%d", entryID)
			if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,modified_at) VALUES(?,?,?,?,?,'DIR',?)", entryID, maximum, maximum-3, name, []byte(name), modifiedAt); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,revision,modified_at) VALUES(?,?,?,'最大文件',CAST('最大文件' AS BINARY),'FILE','OTHER',?,?,?,?)", maximum, maximum, maximum-3, maximum, maximum, maximum, maximumTime); err != nil {
			t.Fatal(err)
		}
		var before *domain.FileEntryPosition
		for index, entryID := range []uint64{maximum, maximum - 1, maximum - 2} {
			page, err := store.ListUserGlobalEntryCandidates(ctx, maximum, globalEntryCandidateQuery(1), before)
			if err != nil {
				t.Fatal(err)
			}
			checkIDs(t, page, []uint64{entryID}, index < 2)
			entry := page.Entries[0]
			if index == 0 && (entry.FileSize != maximum || entry.Revision != maximum || !entry.ModifiedAt.Equal(maximumTime)) {
				t.Fatal("最大整数或时间丢失")
			}
			if index == 2 && !entry.ModifiedAt.Equal(minimumTime) {
				t.Fatal("最早数据库时间丢失")
			}
			before = &domain.FileEntryPosition{ModifiedAt: entry.ModifiedAt, ID: entry.ID}
		}
	})
	t.Run("closed pool is a database error", func(t *testing.T) {
		if err := pool.Close(); err != nil {
			t.Fatal(err)
		}
		page, err := store.ListUserGlobalEntryCandidates(ctx, 1, globalEntryCandidateQuery(2), nil)
		if page != nil || err == nil || errors.Is(err, domain.ErrFileEntryNotFound) {
			t.Fatal("连接故障被当作空列表或目标缺失")
		}
	})
}
