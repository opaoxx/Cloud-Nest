package repository

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestListUserTrashEntryCandidatesIntegration(t *testing.T) {
	// 1. 在独占验证库插入同一微秒的回收项、嵌套项和其他状态及用户的记录。
	pool := userProfileDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	stamp := time.Date(2026, 11, 20, 1, 30, 0, 123456000, time.UTC)
	execute := func(statement string, arguments ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
			t.Fatal("准备回收站候选夹具失败", err)
		}
	}
	execute(`INSERT INTO users(id,username,display_name,password_hash)
		VALUES (1,'fixture_owner','回收用户','fixture_hash'),
		       (3,'fixture_other','其他用户','fixture_hash')`)
	execute(`INSERT INTO file_entries(id,user_id,name,name_key,entry_type)
		VALUES (11,1,'',X'','DIR'), (33,3,'',X'','DIR')`)
	execute(`INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size)
		VALUES (41,1,'MINIO','fixture-bucket','fixture-trash',2048)`)
	execute(`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at)
		VALUES (12,1,11,'回收父目录',CAST('回收父目录' AS BINARY),'DIR','TRASHED',?),
		       (13,1,11,'同刻目录',CAST('同刻目录' AS BINARY),'DIR','TRASHED',?),
		       (99,1,11,'较早目录',CAST('较早目录' AS BINARY),'DIR','TRASHED',?),
		       (31,3,33,'他人目录',CAST('他人目录' AS BINARY),'DIR','TRASHED',?),
		       (16,1,11,'活动目录',CAST('活动目录' AS BINARY),'DIR','ACTIVE',NULL)`,
		stamp.Add(time.Microsecond), stamp, stamp.Add(-time.Microsecond), stamp.Add(time.Second))
	execute(`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at,category,object_id,file_size,revision)
		VALUES (14,1,12,'嵌套文件',CAST('嵌套文件' AS BINARY),'FILE','TRASHED',?,'OTHER',41,2048,3),
		       (15,1,11,'同刻文件',CAST('同刻文件' AS BINARY),'FILE','TRASHED',?,'OTHER',41,2048,2)`, stamp, stamp)
	execute(`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at,purge_started_at,purged_at)
		VALUES (17,1,11,'清理中',CAST('清理中' AS BINARY),'DIR','PURGING',?,?,NULL),
		       (18,1,11,'已清理',CAST('已清理' AS BINARY),'DIR','PURGED',?,?,?)`, stamp, stamp, stamp, stamp, stamp)

	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	checkBatch := func(t *testing.T, readContext context.Context, reader *Store, userID uint64, limit int, before *domain.TrashEntryPosition, ids []uint64, more bool) []domain.TrashedFileEntry {
		t.Helper()
		page, err := reader.ListUserTrashEntryCandidates(readContext, userID, limit, before)
		if err != nil || page == nil || page.Entries == nil {
			t.Fatalf("读取回收候选失败，页为 %+v，错误为 %v", page, err)
		}
		actualIDs := make([]uint64, 0, len(page.Entries))
		for _, entry := range page.Entries {
			if entry.UserID != userID || entry.Status != "TRASHED" || entry.DeletedAt.Location() != time.UTC {
				t.Fatalf("候选归属、状态或回收时间不是预期值，候选为 %+v", entry)
			}
			actualIDs = append(actualIDs, entry.ID)
		}
		if !slices.Equal(actualIDs, ids) || page.HasMore != more {
			t.Fatalf("候选为 %v，HasMore 为 %v，期望 %v 和 %v", actualIDs, page.HasMore, ids, more)
		}
		return page.Entries
	}

	// 2. 验证直接读取的用户边界及回收状态。
	t.Run("user and state", func(t *testing.T) {
		entries := checkBatch(t, ctx, store, 1, 100, nil, []uint64{12, 15, 14, 13, 99}, false)
		nested := entries[2]
		if nested.ParentID == nil || *nested.ParentID != 12 || nested.Name != "嵌套文件" ||
			nested.EntryType != "FILE" || nested.FileSize != 2048 || nested.Revision != 3 || !nested.DeletedAt.Equal(stamp) {
			t.Fatalf("嵌套回收候选的字段错误，候选为 %+v", nested)
		}
		checkBatch(t, ctx, store, 3, 100, nil, []uint64{31}, false)
		checkBatch(t, ctx, store, 9, 100, nil, nil, false)
	})

	// 3. 在只读快照中用上一批末项续读，确认同一回收时间的记录不丢失。
	t.Run("exclusive pagination in snapshot", func(t *testing.T) {
		err := store.WithFileEntrySnapshot(ctx, func(readContext context.Context, reader *Store) error {
			var before *domain.TrashEntryPosition
			offset := time.FixedZone("UTC+08", 8*60*60)
			for index, ids := range [][]uint64{{12, 15}, {14, 13}, {99}, nil} {
				batch := checkBatch(t, readContext, reader, 1, 2, before, ids, index < 2)
				if len(batch) != 0 {
					last := batch[len(batch)-1]
					before = &domain.TrashEntryPosition{ID: last.ID, DeletedAt: last.DeletedAt.In(offset)}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}
