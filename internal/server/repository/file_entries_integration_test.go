package repository

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestFindUserFileEntriesIntegration(t *testing.T) {
	// 1. 使用正式表建立用户、根目录、文件及生命周期夹具。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(2)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash,status) VALUES(1,'fixture_one','批量读取用户','fixture_hash','ACTIVE'),(2,'fixture_disabled','禁用用户','fixture_hash','DISABLED'),(3,'fixture_other','其他用户','fixture_hash','ACTIVE')",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','fixture-batch-object',2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(22,2,NULL,'',X'','DIR'),(33,3,NULL,'',X'','DIR'),(12,1,11,'课程资料 🌱',CAST('课程资料 🌱' AS BINARY),'DIR')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,revision) VALUES(13,1,12,'学习笔记.md',CAST('学习笔记.md' AS BINARY),'FILE','DOCUMENT',41,2048,3),(18,1,12,'另一个文件.md',CAST('另一个文件.md' AS BINARY),'FILE','DOCUMENT',41,2048,1)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at) VALUES(14,1,11,'回收目录',CAST('回收目录' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,purge_started_at) VALUES(15,1,11,'清理目录',CAST('清理目录' AS BINARY),'DIR','PURGING',UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,file_size,status,purged_at) VALUES(16,1,11,'已清理.bin',CAST('已清理.bin' AS BINARY),'FILE','OTHER',64,'PURGED',UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(17,1,14,'回收祖先下活动项',CAST('回收祖先下活动项' AS BINARY),'DIR')",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("建立批量读取夹具失败", err)
		}
	}
	stamp := time.Date(2026, 11, 16, 2, 30, 0, 123456000, time.UTC)
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET created_at=?,modified_at=?,updated_at=?", stamp, stamp.Add(time.Second), stamp.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	checkIDs := func(t *testing.T, entries map[uint64]domain.FileEntry, ids []uint64) {
		t.Helper()
		if entries == nil || len(entries) != len(ids) {
			t.Fatal("批量映射长度或空值错误")
		}
		for _, entryID := range ids {
			entry, found := entries[entryID]
			if !found || entry.ID != entryID {
				t.Fatal("映射标识或记录缺省错误")
			}
		}
	}

	// 2. 验证批量字段、缺省标识、生命周期及独立结果。
	t.Run("complete fields scoped membership and duplicates", func(t *testing.T) {
		entries, err := store.FindUserFileEntries(ctx, 1, []uint64{18, 11, 13, 12, 13, 33, 999})
		if err != nil {
			t.Fatal(err)
		}
		checkIDs(t, entries, []uint64{11, 12, 13, 18})
		for entryID, entry := range entries {
			single, err := store.FindUserFileEntry(ctx, 1, entryID)
			if err != nil || single == nil || !reflect.DeepEqual(entry, *single) {
				t.Fatal("批量记录与单项读取字段不一致", err)
			}
		}
	})
	t.Run("lifecycle and raw ancestor facts", func(t *testing.T) {
		entries, err := store.FindUserFileEntries(ctx, 1, []uint64{14, 15, 16, 17})
		if err != nil {
			t.Fatal(err)
		}
		checkIDs(t, entries, []uint64{14, 15, 16, 17})
		if entries[14].Status != "TRASHED" || entries[15].Status != "PURGING" || entries[16].Status != "PURGED" || entries[16].FileSize != 64 || entries[17].Status != "ACTIVE" || entries[17].ParentID == nil || *entries[17].ParentID != 14 {
			t.Fatal("生命周期或父目录事实被改写")
		}
		disabled, err := store.FindUserFileEntries(ctx, 2, []uint64{22, 11})
		if err != nil {
			t.Fatal(err)
		}
		checkIDs(t, disabled, []uint64{22})
	})
	t.Run("empty missing and foreign batches", func(t *testing.T) {
		for _, fixture := range []struct {
			userID uint64
			ids    []uint64
		}{
			{1, nil}, {1, []uint64{}}, {1, []uint64{33, 999}}, {3, []uint64{13}}, {999, []uint64{11}},
		} {
			entries, err := store.FindUserFileEntries(ctx, fixture.userID, fixture.ids)
			if err != nil {
				t.Fatal(err)
			}
			checkIDs(t, entries, nil)
		}
		foreign, err := store.FindUserFileEntries(ctx, 3, []uint64{33, 11})
		if err != nil {
			t.Fatal(err)
		}
		checkIDs(t, foreign, []uint64{33})
	})
	t.Run("independent map records and parent pointers", func(t *testing.T) {
		first, err := store.FindUserFileEntries(ctx, 1, []uint64{13, 18})
		if err != nil || first[13].ParentID == nil || first[18].ParentID == nil || first[13].ParentID == first[18].ParentID {
			t.Fatal("批量结果共享父目录指针", err)
		}
		entry := first[13]
		*entry.ParentID, entry.Name = 999, "本地副本"
		first[13] = entry
		if *first[18].ParentID != 12 {
			t.Fatal("修改一项影响了其他项")
		}
		delete(first, 18)
		again, err := store.FindUserFileEntries(ctx, 1, []uint64{13, 18})
		if err != nil || len(again) != 2 || again[13].Name != "学习笔记.md" || *again[13].ParentID != 12 {
			t.Fatal("本地映射修改影响后续读取", err)
		}
	})

	// 3. 验证事务连接复用、快照、并发和千项批次。
	t.Run("single connection snapshot", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(2)
		if err := store.WithFileEntrySnapshot(ctx, func(txContext context.Context, txStore *Store) error {
			entries, err := txStore.FindUserFileEntries(txContext, 1, []uint64{11, 12, 13, 14})
			if err != nil {
				return err
			}
			checkIDs(t, entries, []uint64{11, 12, 13, 14})
			return nil
		}); err != nil {
			t.Fatal("批量读取没有复用事务连接", err)
		}
	})
	t.Run("uncommitted write and concurrent readers", func(t *testing.T) {
		before, err := store.FindUserFileEntries(ctx, 1, []uint64{11, 12, 13})
		if err != nil {
			t.Fatal(err)
		}
		writer, err := pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer writer.Rollback()
		if _, err := writer.ExecContext(ctx, "UPDATE file_entries SET name='未提交名称' WHERE id=13"); err != nil {
			t.Fatal(err)
		}
		readContext, release := context.WithTimeout(ctx, time.Second)
		defer release()
		var workers sync.WaitGroup
		for range 8 {
			workers.Go(func() {
				entries, err := store.FindUserFileEntries(readContext, 1, []uint64{11, 12, 13})
				if err != nil || !reflect.DeepEqual(entries, before) {
					t.Error("并发批量读取等待行锁或混入未提交修改", err)
					return
				}
				*entries[13].ParentID = 999
			})
		}
		workers.Wait()
	})
	t.Run("snapshot preserves rename trash insert and delete", func(t *testing.T) {
		ids := []uint64{12, 13, 18, 19}
		if err := store.WithFileEntrySnapshot(ctx, func(txContext context.Context, txStore *Store) error {
			first, err := txStore.FindUserFileEntries(txContext, 1, ids)
			if err != nil {
				return err
			}
			checkIDs(t, first, []uint64{12, 13, 18})
			writer, err := pool.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer writer.Rollback()
			for _, statement := range []string{
				"UPDATE file_entries SET name='已提交新名称.md',name_key=CAST('已提交新名称.md' AS BINARY),revision=revision+1 WHERE id=13",
				"UPDATE file_entries SET status='TRASHED',deleted_at=UTC_TIMESTAMP(6) WHERE id=12",
				"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(19,1,11,'新目录',CAST('新目录' AS BINARY),'DIR')",
				"DELETE FROM file_entries WHERE id=18",
			} {
				if _, err := writer.ExecContext(ctx, statement); err != nil {
					return err
				}
			}
			if err := writer.Commit(); err != nil {
				return err
			}
			again, err := txStore.FindUserFileEntries(txContext, 1, ids)
			if err != nil || !reflect.DeepEqual(first, again) {
				t.Fatal("批量快照混入后续提交", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		current, err := store.FindUserFileEntries(ctx, 1, ids)
		if err != nil {
			t.Fatal(err)
		}
		checkIDs(t, current, []uint64{12, 13, 19})
		if current[12].Status != "TRASHED" || current[13].Name != "已提交新名称.md" || current[13].Revision != 4 {
			t.Fatal("快照结束后的状态事实错误")
		}
	})
	t.Run("maximum batch and duplicate batch", func(t *testing.T) {
		writer, err := pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer writer.Rollback()
		var ids, repeated []uint64
		for index := range domain.MaxGlobalEntryCandidates {
			entryID := uint64(1000 + index)
			name := fmt.Sprintf("batch-%d", index)
			if _, err := writer.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,1,11,?,?,'DIR')", entryID, name, []byte(name)); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, entryID)
			repeated = append(repeated, 13)
		}
		if err := writer.Commit(); err != nil {
			t.Fatal(err)
		}
		entries, err := store.FindUserFileEntries(ctx, 1, ids)
		if err != nil {
			t.Fatal(err)
		}
		checkIDs(t, entries, ids)
		for _, entryID := range ids {
			if entries[entryID].UserID != 1 || entries[entryID].ParentID == nil || *entries[entryID].ParentID != 11 {
				t.Fatal("千项批次存在归属或父目录错误")
			}
		}
		duplicates, err := store.FindUserFileEntries(ctx, 1, repeated)
		if err != nil {
			t.Fatal(err)
		}
		checkIDs(t, duplicates, []uint64{13})
	})

	// 4. 最后验证最大标识、日期边界及连接故障。
	t.Run("full unsigned values and datetime boundaries", func(t *testing.T) {
		maximum := ^uint64(0)
		minimumTime := time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)
		maximumTime := time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
		if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash) VALUES(?,'fixture_max','最大整数用户','fixture_hash')", maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(?,?,'MINIO','fixture-bucket','fixture-max-object',?)", maximum, maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,created_at,modified_at,updated_at) VALUES(?,?,NULL,'',X'','DIR',?,?,?)", maximum-2, maximum, minimumTime, minimumTime, minimumTime); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,?,?,'最大目录',CAST('最大目录' AS BINARY),'DIR')", maximum-1, maximum, maximum-2); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,revision,created_at,modified_at,updated_at) VALUES(?,?,?,'最大文件',CAST('最大文件' AS BINARY),'FILE','OTHER',?,?,?,?,?,?)", maximum, maximum, maximum-1, maximum, maximum, maximum, maximumTime, maximumTime, maximumTime); err != nil {
			t.Fatal(err)
		}
		entries, err := store.FindUserFileEntries(ctx, maximum, []uint64{maximum, maximum - 2, maximum - 1})
		if err != nil {
			t.Fatal(err)
		}
		checkIDs(t, entries, []uint64{maximum, maximum - 1, maximum - 2})
		if entries[maximum].FileSize != maximum || entries[maximum].Revision != maximum || entries[maximum].ParentID == nil || *entries[maximum].ParentID != maximum-1 || !entries[maximum].ModifiedAt.Equal(maximumTime) || entries[maximum-2].ParentID != nil || !entries[maximum-2].CreatedAt.Equal(minimumTime) {
			t.Fatal("最大标识或时间精度丢失")
		}
	})
	t.Run("closed pool errors and empty batch", func(t *testing.T) {
		if err := pool.Close(); err != nil {
			t.Fatal(err)
		}
		entries, err := store.FindUserFileEntries(ctx, 1, []uint64{13})
		if entries != nil || err == nil || errors.Is(err, domain.ErrFileEntryNotFound) {
			t.Fatal("连接故障被当作缺省目录项")
		}
		empty, err := store.FindUserFileEntries(ctx, 1, nil)
		if err != nil {
			t.Fatal(err)
		}
		checkIDs(t, empty, nil)
	})
}
