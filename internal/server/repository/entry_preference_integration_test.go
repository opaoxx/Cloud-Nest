package repository

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestFindUserEntryPreferenceIntegration(t *testing.T) {
	// 1. 复用独占库和正式目录表，增加正式偏好表与各类偏好夹具。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(3)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := strings.Index(migrations.InitialSQL, "CREATE TABLE user_entry_preferences (")
	if start < 0 {
		t.Fatal("未找到正式偏好表")
	}
	end := strings.Index(migrations.InitialSQL[start:], "CREATE TABLE upload_tasks (")
	if end < 0 {
		t.Fatal("未找到正式偏好表结束边界")
	}
	if _, err := pool.ExecContext(ctx, migrations.InitialSQL[start:start+end]); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash,status) VALUES(1,'fixture_one','偏好用户','fixture_hash','ACTIVE'),(2,'fixture_two','禁用用户','fixture_hash','DISABLED'),(3,'fixture_three','其他用户','fixture_hash','ACTIVE')",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','fixture-entry-preference',2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(22,2,NULL,'',X'','DIR'),(33,3,NULL,'',X'','DIR'),(12,1,11,'无偏好目录',CAST('无偏好目录' AS BINARY),'DIR'),(14,1,11,'空偏好目录',CAST('空偏好目录' AS BINARY),'DIR'),(15,1,11,'仅收藏目录',CAST('仅收藏目录' AS BINARY),'DIR'),(16,1,11,'仅访问目录',CAST('仅访问目录' AS BINARY),'DIR')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,revision) VALUES(13,1,12,'偏好笔记.md',CAST('偏好笔记.md' AS BINARY),'FILE','DOCUMENT',41,2048,3)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at) VALUES(17,1,11,'回收目录',CAST('回收目录' AS BINARY),'DIR','TRASHED','2020-01-02 03:04:05.123456')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,purge_started_at) VALUES(18,1,11,'清理目录',CAST('清理目录' AS BINARY),'DIR','PURGING','2020-01-02 03:04:05.123456')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,file_size,status,purged_at) VALUES(19,1,11,'已清理.md',CAST('已清理.md' AS BINARY),'FILE','DOCUMENT',2048,'PURGED','2020-01-02 03:04:05.123456')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(20,1,17,'回收祖先下的子项',CAST('回收祖先下的子项' AS BINARY),'DIR')",
		"INSERT INTO user_entry_preferences(user_id,entry_id,starred_at,last_opened_at) VALUES(1,13,'2020-01-02 03:04:05.123456','2020-01-02 03:04:06.654321'),(1,14,NULL,NULL),(1,15,'2020-01-02 03:04:05.123456',NULL),(1,16,NULL,'2020-01-02 03:04:06.654321'),(1,17,'2020-01-02 03:04:05.123456','2020-01-02 03:04:06.654321'),(1,18,'2020-01-02 03:04:05.123456',NULL),(1,20,'2020-01-02 03:04:05.123456','2020-01-02 03:04:06.654321'),(2,22,'2020-01-02 03:04:05.123456',NULL),(3,33,NULL,'2020-01-02 03:04:06.654321')",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("建立偏好夹具失败", err)
		}
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	starredAt := time.Date(2020, 1, 2, 3, 4, 5, 123456000, time.UTC)
	openedAt := time.Date(2020, 1, 2, 3, 4, 6, 654321000, time.UTC)
	file := domain.EntryPreference{UserID: 1, EntryID: 13, StarredAt: &starredAt, LastOpenedAt: &openedAt}
	check := func(t *testing.T, queryContext context.Context, reader *Store, expected domain.EntryPreference) {
		t.Helper()
		actual, err := reader.FindUserEntryPreference(queryContext, expected.UserID, expected.EntryID)
		if err != nil || !reflect.DeepEqual(actual, &expected) {
			t.Fatal("偏好时间、空值或归属读回错误", err)
		}
	}

	// 2. 核对无偏好默认值、两类时间组合、历史事实及目标归属。
	t.Run("missing preference defaults do not create rows", func(t *testing.T) {
		for _, entryID := range []uint64{11, 12, 19} {
			check(t, ctx, store, domain.EntryPreference{UserID: 1, EntryID: entryID})
			var count int
			if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM user_entry_preferences WHERE user_id=1 AND entry_id=?", entryID).Scan(&count); err != nil || count != 0 {
				t.Fatal("读取默认偏好改变了缺失记录", err)
			}
		}
	})
	t.Run("timestamps and stored historical preferences", func(t *testing.T) {
		for _, expected := range []domain.EntryPreference{
			file,
			{UserID: 1, EntryID: 14},
			{UserID: 1, EntryID: 15, StarredAt: &starredAt},
			{UserID: 1, EntryID: 16, LastOpenedAt: &openedAt},
			{UserID: 1, EntryID: 17, StarredAt: &starredAt, LastOpenedAt: &openedAt},
			{UserID: 1, EntryID: 18, StarredAt: &starredAt},
			{UserID: 1, EntryID: 20, StarredAt: &starredAt, LastOpenedAt: &openedAt},
			{UserID: 2, EntryID: 22, StarredAt: &starredAt},
			{UserID: 3, EntryID: 33, LastOpenedAt: &openedAt},
		} {
			check(t, ctx, store, expected)
		}
	})
	t.Run("missing and foreign targets share not found", func(t *testing.T) {
		for _, pair := range [][2]uint64{{1, 33}, {3, 13}, {3, 12}, {1, 999}, {99, 13}} {
			result, err := store.FindUserEntryPreference(ctx, pair[0], pair[1])
			if result != nil || !errors.Is(err, domain.ErrFileEntryNotFound) {
				t.Fatal("跨账号或缺失目标被当作默认偏好", err)
			}
		}
	})
	t.Run("independent results and read only metadata", func(t *testing.T) {
		before, err := store.FindUserFileEntry(ctx, 1, 13)
		if err != nil {
			t.Fatal(err)
		}
		var createdAt, updatedAt time.Time
		if err := pool.QueryRowContext(ctx, "SELECT created_at,updated_at FROM user_entry_preferences WHERE user_id=1 AND entry_id=13").Scan(&createdAt, &updatedAt); err != nil {
			t.Fatal(err)
		}
		first, err := store.FindUserEntryPreference(ctx, 1, 13)
		if err != nil || first == nil || first.StarredAt == nil || first.LastOpenedAt == nil {
			t.Fatal(err)
		}
		second, err := store.FindUserEntryPreference(ctx, 1, 13)
		if err != nil || !reflect.DeepEqual(second, &file) {
			t.Fatal(err)
		}
		*first.StarredAt, *first.LastOpenedAt = time.Time{}, time.Time{}
		if !reflect.DeepEqual(second, &file) {
			t.Fatal("修改一次结果影响了另一偏好查询")
		}
		check(t, ctx, store, file)
		var createdAfter, updatedAfter time.Time
		if err := pool.QueryRowContext(ctx, "SELECT created_at,updated_at FROM user_entry_preferences WHERE user_id=1 AND entry_id=13").Scan(&createdAfter, &updatedAfter); err != nil || !createdAfter.Equal(createdAt) || !updatedAfter.Equal(updatedAt) {
			t.Fatal("偏好读取修改了记录时间", err)
		}
		after, err := store.FindUserFileEntry(ctx, 1, 13)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("偏好读取修改了目录项版本或时间", err)
		}
	})

	// 3. 普通一致性读取与同一只读快照均正确处理并发偏好变更。
	t.Run("uncommitted preference update does not block read", func(t *testing.T) {
		transaction, err := pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer transaction.Rollback()
		if _, err := transaction.ExecContext(ctx, "UPDATE user_entry_preferences SET starred_at=NULL,last_opened_at=NULL WHERE user_id=1 AND entry_id=13"); err != nil {
			t.Fatal(err)
		}
		readContext, release := context.WithTimeout(ctx, 500*time.Millisecond)
		defer release()
		check(t, readContext, store, file)
	})
	t.Run("snapshot includes existing and missing preference states", func(t *testing.T) {
		if err := store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, reader *Store) error {
			if _, err := reader.FindUserFileEntry(snapshotContext, 1, 13); err != nil {
				return err
			}
			if _, err := pool.ExecContext(ctx, "UPDATE user_entry_preferences SET starred_at=NULL WHERE user_id=1 AND entry_id=13"); err != nil {
				return err
			}
			if _, err := pool.ExecContext(ctx, "INSERT INTO user_entry_preferences(user_id,entry_id,starred_at) VALUES(1,12,?)", starredAt); err != nil {
				return err
			}
			check(t, snapshotContext, reader, file)
			check(t, snapshotContext, reader, domain.EntryPreference{UserID: 1, EntryID: 12})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		check(t, ctx, store, domain.EntryPreference{UserID: 1, EntryID: 13, LastOpenedAt: &openedAt})
		check(t, ctx, store, domain.EntryPreference{UserID: 1, EntryID: 12, StarredAt: &starredAt})
	})

	// 4. 在普通夹具后验证完整标识、微秒日期边界和真实连接故障。
	t.Run("full unsigned ids and database date boundaries", func(t *testing.T) {
		maximum := ^uint64(0)
		minimumDate := time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)
		maximumDate := time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
		if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash) VALUES(?,'fixture_max','完整整数用户','fixture_hash')", maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,?,NULL,'',X'','DIR')", maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO user_entry_preferences(user_id,entry_id,starred_at,last_opened_at) VALUES(?,?,?,?)", maximum, maximum, minimumDate, maximumDate); err != nil {
			t.Fatal(err)
		}
		check(t, ctx, store, domain.EntryPreference{UserID: maximum, EntryID: maximum, StarredAt: &minimumDate, LastOpenedAt: &maximumDate})
	})
	t.Run("closed pool is a database error", func(t *testing.T) {
		if err := pool.Close(); err != nil {
			t.Fatal(err)
		}
		result, err := store.FindUserEntryPreference(ctx, 1, 13)
		if result != nil || err == nil || errors.Is(err, domain.ErrFileEntryNotFound) {
			t.Fatal("连接故障被当作默认偏好或目标不存在")
		}
	})
}
