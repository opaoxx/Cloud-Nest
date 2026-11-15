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

func TestListUserDirectoryEntriesIntegration(t *testing.T) {
	// 1. 正式表独占库中准备不同归属、父目录、状态及相同时间的记录。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(2)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash) VALUES(1,'fixture_one','目录列表用户','fixture_hash'),(3,'fixture_three','其他用户','fixture_hash')",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','fixture-directory-object',2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(33,3,NULL,'',X'','DIR'),(12,1,11,'课程目录',CAST('课程目录' AS BINARY),'DIR'),(14,1,11,'其他目录',CAST('其他目录' AS BINARY),'DIR'),(15,1,11,'批量目录',CAST('批量目录' AS BINARY),'DIR')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at) VALUES(16,1,11,'回收目录',CAST('回收目录' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6)),(106,1,12,'回收子项',CAST('回收子项' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,revision) VALUES(101,1,12,'学习笔记 🌱.md',CAST('学习笔记 🌱.md' AS BINARY),'FILE','DOCUMENT',41,2048,3),(105,1,12,'旧笔记.md',CAST('旧笔记.md' AS BINARY),'FILE','DOCUMENT',41,2048,1)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(102,1,12,'资料二',CAST('资料二' AS BINARY),'DIR'),(103,1,12,'资料三',CAST('资料三' AS BINARY),'DIR'),(104,1,12,'资料四',CAST('资料四' AS BINARY),'DIR'),(109,1,14,'其他父项子目录',CAST('其他父项子目录' AS BINARY),'DIR'),(110,3,33,'其他用户子目录',CAST('其他用户子目录' AS BINARY),'DIR'),(111,1,16,'回收目录中的活动项',CAST('回收目录中的活动项' AS BINARY),'DIR')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,purge_started_at) VALUES(107,1,12,'清理中',CAST('清理中' AS BINARY),'DIR','PURGING',UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,file_size,status,purged_at) VALUES(108,1,12,'已清理.bin',CAST('已清理.bin' AS BINARY),'FILE','OTHER',64,'PURGED',UTC_TIMESTAMP(6))",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("建立目录列表夹具失败", err)
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
			t.Fatal("目录页长度、空数组或 HasMore 错误")
		}
		for index, entry := range page.Entries {
			if entry.ID != expected[index] || entry.Status != "ACTIVE" {
				t.Fatal("目录页顺序或状态错误")
			}
		}
	}

	// 2. 跨过相同时间的多页，保持修改时间/ID 排序及用户、父目录隔离。
	t.Run("stable tuple pagination and complete fields", func(t *testing.T) {
		var before *domain.FileEntryPosition
		var allIDs []uint64
		for index, expected := range [][]uint64{{102, 101}, {104, 103}, {105}} {
			page, err := store.ListUserDirectoryEntries(ctx, 1, 12, before, 2)
			if err != nil {
				t.Fatal(err)
			}
			checkIDs(t, page, expected, index < 2)
			for _, entry := range page.Entries {
				stored, err := store.FindUserFileEntry(ctx, 1, entry.ID)
				if err != nil || stored == nil || !reflect.DeepEqual(entry, *stored) {
					t.Fatal("列表基础字段与数据库事实不一致", err)
				}
				allIDs = append(allIDs, entry.ID)
			}
			last := page.Entries[len(page.Entries)-1]
			before = &domain.FileEntryPosition{ModifiedAt: last.ModifiedAt, ID: last.ID}
		}
		if !reflect.DeepEqual(allIDs, []uint64{102, 101, 104, 103, 105}) {
			t.Fatal("跨页漏项或重复")
		}
		empty, err := store.ListUserDirectoryEntries(ctx, 1, 12, before, 2)
		if err != nil {
			t.Fatal(err)
		}
		checkIDs(t, empty, nil, false)
		page, err := store.ListUserDirectoryEntries(ctx, 1, 12, nil, 10)
		if err != nil {
			t.Fatal(err)
		}
		if page.Entries[0].ParentID == page.Entries[1].ParentID {
			t.Fatal("列表记录共享父目录指针")
		}
		page.Entries[0].Name, *page.Entries[0].ParentID = "本地修改", 99
		again, err := store.ListUserDirectoryEntries(ctx, 1, 12, nil, 10)
		if err != nil || again.Entries[0].Name != "资料二" || *again.Entries[0].ParentID != 12 {
			t.Fatal("修改本地页影响了后续读取", err)
		}
	})
	t.Run("scope empty pages and parent lifecycle facts", func(t *testing.T) {
		for _, testCase := range []struct {
			userID, parentID uint64
			ids              []uint64
		}{
			{userID: 1, parentID: 14, ids: []uint64{109}},
			{userID: 3, parentID: 33, ids: []uint64{110}},
			{userID: 1, parentID: 33},
			{userID: 3, parentID: 12},
			{userID: 1, parentID: 15},
			{userID: 1, parentID: 999},
			{userID: 1, parentID: 16, ids: []uint64{111}},
		} {
			page, err := store.ListUserDirectoryEntries(ctx, testCase.userID, testCase.parentID, nil, 10)
			if err != nil {
				t.Fatal(err)
			}
			checkIDs(t, page, testCase.ids, false)
		}
		rootPage, err := store.ListUserDirectoryEntries(ctx, 1, 11, nil, 10)
		if err != nil {
			t.Fatal(err)
		}
		checkIDs(t, rootPage, []uint64{15, 14, 12}, false)
	})
	t.Run("maximum page size", func(t *testing.T) {
		for index := 0; index < 101; index++ {
			name := fmt.Sprintf("bulk-%d", index)
			if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,modified_at) VALUES(?,1,15,?,?,'DIR',?)", 1000+index, name, []byte(name), stamp); err != nil {
				t.Fatal(err)
			}
		}
		first, err := store.ListUserDirectoryEntries(ctx, 1, 15, nil, 100)
		if err != nil || first == nil || len(first.Entries) != 100 || !first.HasMore || first.Entries[0].ID != 1100 || first.Entries[99].ID != 1001 {
			t.Fatal("百条页及额外记录错误", err)
		}
		last := first.Entries[99]
		second, err := store.ListUserDirectoryEntries(ctx, 1, 15, &domain.FileEntryPosition{ModifiedAt: last.ModifiedAt, ID: last.ID}, 100)
		if err != nil {
			t.Fatal(err)
		}
		checkIDs(t, second, []uint64{1000}, false)
	})

	// 3. 在并发写入与快照中验证同一读取视图，事务外继续使用复合位置翻页。
	t.Run("uncommitted write preserves committed page", func(t *testing.T) {
		before, err := store.ListUserDirectoryEntries(ctx, 1, 12, nil, 10)
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
		current, err := store.ListUserDirectoryEntries(readContext, 1, 12, nil, 10)
		if err != nil || !reflect.DeepEqual(before, current) {
			t.Fatal("未提交写入影响了已提交页或等待了行锁", err)
		}
	})
	t.Run("snapshot retains page across insert and trash", func(t *testing.T) {
		if err := store.WithFileEntrySnapshot(ctx, func(txContext context.Context, txStore *Store) error {
			first, err := txStore.ListUserDirectoryEntries(txContext, 1, 12, nil, 2)
			if err != nil {
				return err
			}
			checkIDs(t, first, []uint64{102, 101}, true)
			writer, err := pool.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer writer.Rollback()
			if _, err := writer.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,modified_at) VALUES(112,1,12,'新子项',CAST('新子项' AS BINARY),'DIR',?)", stamp.Add(9*time.Second)); err != nil {
				return err
			}
			if _, err := writer.ExecContext(ctx, "UPDATE file_entries SET status='TRASHED',deleted_at=UTC_TIMESTAMP(6),modified_at=UTC_TIMESTAMP(6) WHERE id=102"); err != nil {
				return err
			}
			if err := writer.Commit(); err != nil {
				return err
			}
			again, err := txStore.ListUserDirectoryEntries(txContext, 1, 12, nil, 2)
			if err != nil || !reflect.DeepEqual(first, again) {
				t.Fatal("同一快照混入新增或回收状态", err)
			}
			last := first.Entries[1]
			next, err := txStore.ListUserDirectoryEntries(txContext, 1, 12, &domain.FileEntryPosition{ModifiedAt: last.ModifiedAt, ID: last.ID}, 2)
			if err != nil {
				return err
			}
			checkIDs(t, next, []uint64{104, 103}, true)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		current, err := store.ListUserDirectoryEntries(ctx, 1, 12, nil, 2)
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
			page, err := store.ListUserDirectoryEntries(ctx, maximum, maximum-3, before, 1)
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
		page, err := store.ListUserDirectoryEntries(ctx, 1, 12, nil, 2)
		if page != nil || err == nil || errors.Is(err, domain.ErrFileEntryNotFound) {
			t.Fatal("连接故障被当作空列表或目标缺失")
		}
	})
}
