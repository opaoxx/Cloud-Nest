package repository

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

type visibilitySnapshotReader struct {
	*Store
	profiles  int
	batches   [][]uint64
	afterRead func() error
}

func (reader *visibilitySnapshotReader) FindUserProfile(ctx context.Context, userID uint64) (*domain.UserProfile, error) {
	reader.profiles++
	return reader.Store.FindUserProfile(ctx, userID)
}

func (reader *visibilitySnapshotReader) FindUserFileEntries(ctx context.Context, userID uint64, ids []uint64) (map[uint64]domain.FileEntry, error) {
	reader.batches = append(reader.batches, slices.Clone(ids))
	entries, err := reader.Store.FindUserFileEntries(ctx, userID, ids)
	if err == nil && reader.afterRead != nil {
		err = reader.afterRead()
	}
	return entries, err
}

func TestFilterVisibleFileEntriesIntegration(t *testing.T) {
	// 1. 准备真实候选、共享祖先、隐藏父链和不同账号。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(2)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash,status) VALUES(1,'fixture_one','可见性用户','fixture_hash','ACTIVE'),(3,'fixture_other','其他用户','fixture_hash','ACTIVE'),(5,'fixture_disabled','禁用用户','fixture_hash','DISABLED')",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','fixture-visible-object',2048),(43,3,'MINIO','fixture-bucket','fixture-other-object',2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(33,3,NULL,'',X'','DIR'),(55,5,NULL,'',X'','DIR'),(12,1,11,'课程目录',CAST('课程目录' AS BINARY),'DIR'),(14,1,11,'其他目录',CAST('其他目录' AS BINARY),'DIR')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at) VALUES(16,1,11,'回收父目录',CAST('回收父目录' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(21,1,16,'活动中间目录',CAST('活动中间目录' AS BINARY),'DIR')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(13,1,12,'学习笔记.md',CAST('学习笔记.md' AS BINARY),'FILE','DOCUMENT',41,2048),(15,1,14,'课程总结.md',CAST('课程总结.md' AS BINARY),'FILE','DOCUMENT',41,2048),(22,1,21,'隐藏文件.md',CAST('隐藏文件.md' AS BINARY),'FILE','DOCUMENT',41,2048),(43,3,33,'其他账号.md',CAST('其他账号.md' AS BINARY),'FILE','DOCUMENT',43,2048)",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("建立可见性夹具失败", err)
		}
	}
	stamp := time.Date(2026, 11, 16, 2, 30, 0, 123456000, time.UTC)
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET created_at=?,modified_at=?,updated_at=?", stamp, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET modified_at=? WHERE id IN (13,15)", stamp.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Actor{UserID: 1, DeviceID: 19, SessionID: 17}
	readVisible := func(currentActor domain.Actor, query domain.GlobalEntryQuery, afterRead func() error) ([]domain.FileEntry, *visibilitySnapshotReader, error) {
		var prepared []domain.FileEntry
		var reader *visibilitySnapshotReader
		err := store.WithFileEntrySnapshot(ctx, func(txContext context.Context, txStore *Store) error {
			page, err := txStore.ListUserGlobalEntryCandidates(txContext, currentActor.UserID, query, nil)
			if err != nil {
				return err
			}
			reader = &visibilitySnapshotReader{Store: txStore, afterRead: afterRead}
			prepared, err = service.FilterVisibleFileEntries(txContext, currentActor, page.Entries, reader)
			return err
		})
		if err != nil {
			return nil, reader, err
		}
		return prepared, reader, nil
	}
	checkIDs := func(t *testing.T, entries []domain.FileEntry, ids []uint64) {
		t.Helper()
		if entries == nil || len(entries) != len(ids) {
			t.Fatal("可见列表长度或空切片错误")
		}
		for index, entryID := range ids {
			if entries[index].ID != entryID || entries[index].Status != "ACTIVE" {
				t.Fatal("可见项顺序或状态错误")
			}
		}
	}

	// 2. 在同一快照内组合候选读取、批量补齐和路径检查。
	t.Run("query visibility order and shared ancestors", func(t *testing.T) {
		entries, reader, err := readVisible(actor, globalEntryCandidateQuery(100), nil)
		if err != nil {
			t.Fatal(err)
		}
		checkIDs(t, entries, []uint64{15, 13, 14, 12})
		if reader.profiles != 1 || !reflect.DeepEqual(reader.batches, [][]uint64{{16, 11}}) {
			t.Fatal("共享祖先未去重或资料重复读取", reader.batches)
		}
		for _, entry := range entries {
			stored, err := store.FindUserFileEntry(ctx, actor.UserID, entry.ID)
			if err != nil || stored == nil || !reflect.DeepEqual(entry, *stored) {
				t.Fatal("可见结果与正式表基础记录不一致", err)
			}
		}
	})
	t.Run("classification and search retain order", func(t *testing.T) {
		query := globalEntryCandidateQuery(100)
		query.Kind = "document"
		entries, _, err := readVisible(actor, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		checkIDs(t, entries, []uint64{15, 13})
		query.Search = "学习"
		entries, _, err = readVisible(actor, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		checkIDs(t, entries, []uint64{13})
	})
	t.Run("hidden target direct batch", func(t *testing.T) {
		if err := store.WithFileEntrySnapshot(ctx, func(txContext context.Context, txStore *Store) error {
			entries, err := txStore.FindUserFileEntries(txContext, 1, []uint64{16})
			if err != nil {
				return err
			}
			result, err := service.FilterVisibleFileEntries(txContext, actor, []domain.FileEntry{entries[16]}, txStore)
			if err != nil {
				return err
			}
			checkIDs(t, result, nil)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	// 3. 验证状态、归属、单连接及并发父链变更。
	t.Run("hidden parent lifecycle", func(t *testing.T) {
		for _, status := range []string{"TRASHED", "PURGING", "PURGED"} {
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status=?,deleted_at=UTC_TIMESTAMP(6),purge_started_at=IF(?='TRASHED',NULL,UTC_TIMESTAMP(6)),purged_at=IF(?='PURGED',UTC_TIMESTAMP(6),NULL) WHERE id=12", status, status, status); err != nil {
				t.Fatal(err)
			}
			query := globalEntryCandidateQuery(100)
			query.Kind = "document"
			entries, _, err := readVisible(actor, query, nil)
			if err != nil {
				t.Fatal(err)
			}
			checkIDs(t, entries, []uint64{15})
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='ACTIVE',deleted_at=NULL,purge_started_at=NULL,purged_at=NULL WHERE id=12"); err != nil {
				t.Fatal(err)
			}
		}
	})
	t.Run("account scope and empty candidates", func(t *testing.T) {
		other := actor
		other.UserID = 3
		entries, _, err := readVisible(other, globalEntryCandidateQuery(100), nil)
		if err != nil {
			t.Fatal(err)
		}
		checkIDs(t, entries, []uint64{43})
		query := globalEntryCandidateQuery(100)
		query.Search = "无匹配项"
		entries, _, err = readVisible(actor, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		checkIDs(t, entries, nil)
		other.UserID = 5
		entries, _, err = readVisible(other, query, nil)
		if entries != nil || !errors.Is(err, domain.ErrAccountDisabled) {
			t.Fatal("空候选未复查禁用账号", err)
		}
		other.UserID = 999
		entries, _, err = readVisible(other, query, nil)
		if entries != nil || !errors.Is(err, domain.ErrAccountNotFound) {
			t.Fatal("缺失账号错误不符", err)
		}
	})
	t.Run("single connection snapshot", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(2)
		entries, _, err := readVisible(actor, globalEntryCandidateQuery(100), nil)
		if err != nil {
			t.Fatal(err)
		}
		checkIDs(t, entries, []uint64{15, 13, 14, 12})
	})
	t.Run("snapshot preserves parent move", func(t *testing.T) {
		query := globalEntryCandidateQuery(100)
		query.Kind = "document"
		changed := false
		entries, _, err := readVisible(actor, query, func() error {
			if changed {
				return nil
			}
			changed = true
			_, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=16 WHERE id=12")
			return err
		})
		if err != nil || !changed {
			t.Fatal("快照并发变更失败", err)
		}
		checkIDs(t, entries, []uint64{15, 13})
		current, _, err := readVisible(actor, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		checkIDs(t, current, []uint64{15})
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=11 WHERE id=12"); err != nil {
			t.Fatal(err)
		}
	})

	// 4. 验证最大深度与异常父链。
	t.Run("cycle and file ancestor", func(t *testing.T) {
		for _, parentID := range []uint64{12, 15} {
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=? WHERE id=12", parentID); err != nil {
				t.Fatal(err)
			}
			query := globalEntryCandidateQuery(100)
			query.Search = "学习笔记"
			entries, _, err := readVisible(actor, query, nil)
			if entries != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
				t.Fatal("循环或文件祖先未拒绝", err)
			}
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=11 WHERE id=12"); err != nil {
				t.Fatal(err)
			}
		}
	})
	t.Run("depth 32 and 33", func(t *testing.T) {
		parentID := uint64(11)
		for level := 1; level <= 31; level++ {
			entryID := uint64(1000 + level)
			name := fmt.Sprintf("level-%d", level)
			if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,1,?,?,?,'DIR')", entryID, parentID, name, []byte(name)); err != nil {
				t.Fatal(err)
			}
			parentID = entryID
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(2000,1,?,'深层文件.md',CAST('深层文件.md' AS BINARY),'FILE','DOCUMENT',41,2048)", parentID); err != nil {
			t.Fatal(err)
		}
		query := globalEntryCandidateQuery(100)
		query.Search, query.Kind = "深层文件", "document"
		entries, reader, err := readVisible(actor, query, nil)
		if err != nil || len(reader.batches) != 32 {
			t.Fatal("合法深度或批次轮数不符", err)
		}
		checkIDs(t, entries, []uint64{2000})
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(1032,1,?,'level-32',CAST('level-32' AS BINARY),'DIR')", parentID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=1032 WHERE id=2000"); err != nil {
			t.Fatal(err)
		}
		entries, _, err = readVisible(actor, query, nil)
		if entries != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
			t.Fatal("超过深度上限未拒绝", err)
		}
	})
}
