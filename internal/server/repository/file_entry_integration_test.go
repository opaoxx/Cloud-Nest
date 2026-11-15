package repository

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestFindUserFileEntryIntegration(t *testing.T) {
	// 1. 复用正式目录项表和独占库，建立根目录、普通目录及文件夹具。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(2)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash,status) VALUES(1,'fixture_one','目录项用户','fixture_hash','ACTIVE'),(2,'fixture_two','禁用用户','fixture_hash','DISABLED'),(3,'fixture_three','其他用户','fixture_hash','ACTIVE')",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','fixture-object',2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(22,2,NULL,'',X'','DIR'),(33,3,NULL,'',X'','DIR'),(12,1,11,'课程资料 🌱',CAST('课程资料 🌱' AS BINARY),'DIR')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,revision) VALUES(13,1,12,'学习笔记.md',CAST('学习笔记.md' AS BINARY),'FILE','DOCUMENT',41,2048,3)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at) VALUES(14,1,11,'回收目录',CAST('回收目录' AS BINARY),'DIR','TRASHED','2020-01-02 03:04:05.123456')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,purge_started_at) VALUES(15,1,11,'清理目录',CAST('清理目录' AS BINARY),'DIR','PURGING','2020-01-02 03:04:05.123456')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,file_size,status,purged_at) VALUES(16,1,11,'已清理.md',CAST('已清理.md' AS BINARY),'FILE','DOCUMENT',2048,'PURGED','2020-01-02 03:04:05.123456')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(17,1,14,'回收目录中的子项',CAST('回收目录中的子项' AS BINARY),'DIR')",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("建立目录项夹具失败", err)
		}
	}
	stamp := time.Date(2020, 1, 2, 3, 4, 5, 123456000, time.UTC)
	modifiedAt, updatedAt := stamp.Add(time.Second), stamp.Add(2*time.Second)
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET created_at=?,modified_at=?,updated_at=?", stamp, modifiedAt, updatedAt); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	check := func(t *testing.T, queryContext context.Context, expected domain.FileEntry) {
		t.Helper()
		actual, err := store.FindUserFileEntry(queryContext, expected.UserID, expected.ID)
		if err != nil || !reflect.DeepEqual(actual, &expected) {
			t.Fatal("目录项基础字段读回错误", err)
		}
	}
	parentID := uint64(12)
	file := domain.FileEntry{ID: 13, UserID: 1, ParentID: &parentID, Name: "学习笔记.md", EntryType: "FILE", FileSize: 2048, Revision: 3, Status: "ACTIVE", CreatedAt: stamp, ModifiedAt: modifiedAt, UpdatedAt: updatedAt}

	// 2. 核对全部投影、生命周期事实、归属隔离及只读行为。
	t.Run("root directory file and independent copies", func(t *testing.T) {
		check(t, ctx, domain.FileEntry{ID: 11, UserID: 1, Name: "", EntryType: "DIR", Revision: 1, Status: "ACTIVE", CreatedAt: stamp, ModifiedAt: modifiedAt, UpdatedAt: updatedAt})
		rootID := uint64(11)
		check(t, ctx, domain.FileEntry{ID: 12, UserID: 1, ParentID: &rootID, Name: "课程资料 🌱", EntryType: "DIR", Revision: 1, Status: "ACTIVE", CreatedAt: stamp, ModifiedAt: modifiedAt, UpdatedAt: updatedAt})
		check(t, ctx, file)
		first, err := store.FindUserFileEntry(ctx, 1, 13)
		if err != nil || first == nil || first.ParentID == nil {
			t.Fatal(err)
		}
		second, err := store.FindUserFileEntry(ctx, 1, 13)
		if err != nil || second == nil || second.ParentID == nil || first == second || first.ParentID == second.ParentID {
			t.Fatal("重复查询共享了可变结果", err)
		}
		first.Name, *first.ParentID = "本地修改", 999
		if !reflect.DeepEqual(second, &file) {
			t.Fatal("修改一次结果影响了另一查询")
		}
		check(t, ctx, file)
		var revision uint64
		var created, modified, updated time.Time
		if err := pool.QueryRowContext(ctx, "SELECT revision,created_at,modified_at,updated_at FROM file_entries WHERE id=13").Scan(&revision, &created, &modified, &updated); err != nil || revision != 3 || created != stamp || modified != modifiedAt || updated != updatedAt {
			t.Fatal("只读查询改变了版本或时间", err)
		}
	})
	t.Run("lifecycle and ancestor facts preserved", func(t *testing.T) {
		for _, testCase := range []struct {
			id, parentID uint64
			name, status string
		}{
			{id: 14, parentID: 11, name: "回收目录", status: "TRASHED"},
			{id: 15, parentID: 11, name: "清理目录", status: "PURGING"},
			{id: 17, parentID: 14, name: "回收目录中的子项", status: "ACTIVE"},
		} {
			check(t, ctx, domain.FileEntry{ID: testCase.id, UserID: 1, ParentID: &testCase.parentID, Name: testCase.name, EntryType: "DIR", Revision: 1, Status: testCase.status, CreatedAt: stamp, ModifiedAt: modifiedAt, UpdatedAt: updatedAt})
		}
		purged := file
		rootID := uint64(11)
		purged.ID, purged.ParentID, purged.Name, purged.Revision, purged.Status = 16, &rootID, "已清理.md", 1, "PURGED"
		check(t, ctx, purged)
		check(t, ctx, domain.FileEntry{ID: 22, UserID: 2, EntryType: "DIR", Revision: 1, Status: "ACTIVE", CreatedAt: stamp, ModifiedAt: modifiedAt, UpdatedAt: updatedAt})
	})
	t.Run("missing and foreign entries share not found", func(t *testing.T) {
		for _, pair := range [][2]uint64{{1, 33}, {3, 13}, {1, 999}, {99, 13}} {
			result, err := store.FindUserFileEntry(ctx, pair[0], pair[1])
			if result != nil || !errors.Is(err, domain.ErrFileEntryNotFound) {
				t.Fatal("归属或缺失结果错误", err)
			}
		}
	})

	// 3. 普通一致性读取取得已提交快照，外部写锁期间也按期限完成。
	t.Run("consistent read under uncommitted write", func(t *testing.T) {
		transaction, err := pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer transaction.Rollback()
		if _, err := transaction.ExecContext(ctx, "UPDATE file_entries SET name='提交后的笔记.md',revision=revision+1,modified_at=UTC_TIMESTAMP(6),updated_at=UTC_TIMESTAMP(6) WHERE id=13"); err != nil {
			t.Fatal(err)
		}
		readContext, release := context.WithTimeout(ctx, 500*time.Millisecond)
		defer release()
		check(t, readContext, file)
		if err := transaction.Commit(); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRowContext(ctx, "SELECT name,revision,modified_at,updated_at FROM file_entries WHERE id=13").Scan(&file.Name, &file.Revision, &file.ModifiedAt, &file.UpdatedAt); err != nil {
			t.Fatal(err)
		}
		check(t, ctx, file)
	})

	// 4. 最大自增 ID 放在普通夹具之后，核对完整无符号范围及连接故障。
	t.Run("full unsigned bigint range", func(t *testing.T) {
		maximum := ^uint64(0)
		if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash) VALUES(?,'fixture_max','完整整数用户','fixture_hash')", maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,created_at,modified_at,updated_at) VALUES(?,?,NULL,'',X'','DIR',?,?,?)", maximum, maximum, stamp, modifiedAt, updatedAt); err != nil {
			t.Fatal(err)
		}
		check(t, ctx, domain.FileEntry{ID: maximum, UserID: maximum, EntryType: "DIR", Revision: 1, Status: "ACTIVE", CreatedAt: stamp, ModifiedAt: modifiedAt, UpdatedAt: updatedAt})
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,file_size,revision,status,purged_at,created_at,modified_at,updated_at) VALUES(?,?,?,'最大值.bin',CAST('最大值.bin' AS BINARY),'FILE','OTHER',?,?,'PURGED',?,?,?,?)", maximum-1, maximum, maximum, maximum, maximum, stamp, stamp, modifiedAt, updatedAt); err != nil {
			t.Fatal(err)
		}
		check(t, ctx, domain.FileEntry{ID: maximum - 1, UserID: maximum, ParentID: &maximum, Name: "最大值.bin", EntryType: "FILE", FileSize: maximum, Revision: maximum, Status: "PURGED", CreatedAt: stamp, ModifiedAt: modifiedAt, UpdatedAt: updatedAt})
	})
	t.Run("closed pool is a database error", func(t *testing.T) {
		if err := pool.Close(); err != nil {
			t.Fatal(err)
		}
		result, err := store.FindUserFileEntry(ctx, 1, 13)
		if result != nil || err == nil || errors.Is(err, domain.ErrFileEntryNotFound) {
			t.Fatal("连接池故障被当作目录项不存在")
		}
	})
}
