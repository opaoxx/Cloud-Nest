package repository

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

func trashEntryTestDatabase(t *testing.T, ctx context.Context) (*sql.DB, *Store, domain.Actor) {
	t.Helper()
	// 1. 在独占库安装正式表，通过单连接验证读取能力来自当前事务。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(1)
	start := strings.Index(migrations.InitialSQL, "CREATE TABLE auth_sessions (")
	end := strings.Index(migrations.InitialSQL, "CREATE TABLE stored_objects (")
	if start < 0 || end <= start {
		t.Fatal("会话表迁移未找到")
	}
	for _, statement := range []string{
		migrations.InitialSQL[start:end],
		"INSERT INTO users(id,username,display_name,password_hash) VALUES(1,'trash_owner','回收用户','fixture_hash'),(2,'trash_other','其他用户','fixture_hash')",
		"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(19,1,'28ee758a-0727-42de-ae7d-3bb33155e148','回收设备','WEB')",
		"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(17,1,19,UNHEX(REPEAT('11',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','trash-target',2048)",
		`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,revision)
		 VALUES(11,1,NULL,'',X'','DIR',1),(21,2,NULL,'',X'','DIR',1),
		       (12,1,11,'正常目录',CAST('正常目录' AS BINARY),'DIR',1),
		       (16,1,11,'最高版本',CAST('最高版本' AS BINARY),'DIR',18446744073709551615)`,
		`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at)
		 VALUES(14,1,11,'已回收目录',CAST('已回收目录' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6))`,
		`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,revision)
		 VALUES(13,1,12,'有效.md',CAST('有效.md' AS BINARY),'FILE','DOCUMENT',41,2048,3),
		       (15,1,14,'隐藏.md',CAST('隐藏.md' AS BINARY),'FILE','DOCUMENT',41,2048,5)`,
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("创建回收目标夹具失败", err)
		}
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Actor{UserID: 1, DeviceID: 19, SessionID: 17}
	return pool, store, actor
}

func TestValidateTrashEntryTargetIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	_, store, actor := trashEntryTestDatabase(t, ctx)
	readEntries := func() map[uint64]*domain.FileEntry {
		t.Helper()
		entries := make(map[uint64]*domain.FileEntry)
		for _, entryID := range []uint64{11, 12, 13, 14, 15, 16} {
			entry, err := store.FindUserFileEntry(ctx, actor.UserID, entryID)
			if err != nil {
				t.Fatal("读回目录项失败", err)
			}
			entries[entryID] = entry
		}
		return entries
	}
	before := readEntries()

	// 2. 在已认证用户事务中检查正常目标及拒绝条件。
	for _, scenario := range []struct {
		name              string
		entryID, revision uint64
		want              error
	}{
		{"file", 13, 3, nil},
		{"directory", 12, 1, nil},
		{"real root", 11, 2, domain.ErrRootEntryTrash},
		{"foreign target", 21, 1, domain.ErrFileEntryNotFound},
		{"missing target", 99, 1, domain.ErrFileEntryNotFound},
		{"already trashed", 14, 1, domain.ErrFileEntryNotFound},
		{"active child of trashed directory", 15, 4, domain.ErrFileEntryNotFound},
		{"stale revision", 13, 2, domain.ErrEntryRevisionConflict},
		{"exhausted revision", 16, math.MaxUint64, domain.ErrEntryRevisionExhausted},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var entry *domain.FileEntry
			err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				var checkError error
				entry, checkError = service.ValidateTrashEntryTarget(txContext, actor, scenario.entryID, scenario.revision, txStore)
				return checkError
			})
			if !errors.Is(err, scenario.want) {
				t.Fatal("回收目标事务结果不符", err)
			}
			if scenario.want == nil {
				if !reflect.DeepEqual(entry, before[scenario.entryID]) {
					t.Fatal("回收目标与 MySQL 记录不符", entry)
				}
				return
			}
			if entry != nil {
				t.Fatal("拒绝后仍交付可写目标", entry)
			}
			if scenario.want == domain.ErrEntryRevisionConflict {
				var conflict *domain.EntryRevisionConflictError
				if !errors.As(err, &conflict) || conflict.CurrentRevision != before[scenario.entryID].Revision {
					t.Fatal("冲突未保留事务中读到的实际版本", err)
				}
			}
		})
	}

	// 3. 验证整个前置检查只读取目录项。
	if after := readEntries(); !reflect.DeepEqual(after, before) {
		t.Fatal("回收目标检查改变了目录项事实")
	}
}
