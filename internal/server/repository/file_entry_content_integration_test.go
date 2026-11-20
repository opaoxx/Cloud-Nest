package repository

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestFindUserFileEntryContentIntegration(t *testing.T) {
	// 1. 在独占库使用正式表定义建立目录、文件和当前对象引用。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(2)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash) VALUES(1,'fixture_owner','文件所有者','fixture_hash'),(2,'fixture_other','其他用户','fixture_hash')",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','old-object',4),(42,1,'MINIO','fixture-bucket','new-object',8)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(22,2,NULL,'',X'','DIR')",
		`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,mime_type)
		 VALUES(13,1,11,'image.png',CAST('image.png' AS BINARY),'FILE','IMAGE',41,4,'image/png'),
		       (14,1,11,'legacy.bin',CAST('legacy.bin' AS BINARY),'FILE','OTHER',42,8,NULL)`,
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,status,purged_at) VALUES(15,1,11,'purged.bin',CAST('purged.bin' AS BINARY),'FILE','OTHER','PURGED',UTC_TIMESTAMP(6))",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("建立内容引用夹具失败", err)
		}
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	assertContent := func(t *testing.T, queryContext context.Context, reader *Store, want domain.FileEntryContent) *domain.FileEntryContent {
		t.Helper()
		result, err := reader.FindUserFileEntryContent(queryContext, want.UserID, want.ID)
		if err != nil || !reflect.DeepEqual(result, &want) {
			t.Fatal("目录项内容引用与预期不符", result, err)
		}
		return result
	}
	original := domain.FileEntryContent{ID: 13, UserID: 1, ObjectID: new(uint64(41)), MIMEType: new("image/png")}

	// 2. 读取真实引用及合法空值，缺失与跨账号保持相同错误。
	t.Run("references nulls and owner scope", func(t *testing.T) {
		first := assertContent(t, ctx, store, original)
		*first.ObjectID, *first.MIMEType = 99, "changed"
		assertContent(t, ctx, store, original)
		assertContent(t, ctx, store, domain.FileEntryContent{ID: 14, UserID: 1, ObjectID: new(uint64(42))})
		assertContent(t, ctx, store, domain.FileEntryContent{ID: 11, UserID: 1})
		assertContent(t, ctx, store, domain.FileEntryContent{ID: 15, UserID: 1})
		for _, target := range []struct{ userID, entryID uint64 }{{2, 13}, {1, 999}} {
			if result, err := store.FindUserFileEntryContent(ctx, target.userID, target.entryID); result != nil || !errors.Is(err, domain.ErrFileEntryNotFound) {
				t.Fatal("缺失或跨账号未统一返回未找到", err)
			}
		}
	})

	// 3. 基础记录和内容引用共用快照，新查询才能看到替换后的对象。
	t.Run("same snapshot as entry metadata", func(t *testing.T) {
		err := store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, snapshot *Store) error {
			entry, err := snapshot.FindUserFileEntry(snapshotContext, 1, 13)
			if err != nil || entry == nil || entry.Revision != 1 || entry.FileSize != 4 {
				t.Fatal("未从快照取得原目录项", entry, err)
			}
			if _, err := pool.ExecContext(snapshotContext, "UPDATE file_entries SET object_id=42,file_size=8,mime_type=NULL,revision=2 WHERE id=13"); err != nil {
				return err
			}
			assertContent(t, snapshotContext, snapshot, original)
			return nil
		})
		if err != nil {
			t.Fatal("读取同一快照失败", err)
		}
		assertContent(t, ctx, store, domain.FileEntryContent{ID: 13, UserID: 1, ObjectID: new(uint64(42))})
	})

	// 4. 单独写入最大无符号标识，核对真实连接不截断查询参数或内容引用。
	t.Run("full unsigned identifiers", func(t *testing.T) {
		const maximum uint64 = math.MaxUint64
		if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash) VALUES(?,'fixture_max','最大标识用户','fixture_hash')", maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(?,?,'MINIO','fixture-bucket','max-object',0)", maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,?,NULL,'',X'','DIR')", maximum-1, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id) VALUES(?,?,?,'max.bin',CAST('max.bin' AS BINARY),'FILE','OTHER',?)", maximum, maximum, maximum-1, maximum); err != nil {
			t.Fatal(err)
		}
		assertContent(t, ctx, store, domain.FileEntryContent{ID: maximum, UserID: maximum, ObjectID: new(maximum)})
	})
}
