package repository

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestListSyncSnapshotEntriesIntegration(t *testing.T) {
	// 1. 建立独占夹具，覆盖空树、活动分支和隐藏父目录下的活动条目。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(2)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	exec := func(t *testing.T, statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal("写入隔离夹具失败", err)
		}
	}
	exec(t, `INSERT INTO users(id,username,display_name,password_hash,change_seq) VALUES
		(1,'snapshot_tree','Tree','fixture_hash',20),(2,'snapshot_empty','Empty','fixture_hash',0),
		(3,'snapshot_wide','Wide','fixture_hash',0)`)
	digest := sha256.Sum256([]byte(strings.Repeat("a", 123)))
	exec(t, `INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size,status,verified_sha256,verified_at)
		VALUES (41,1,'MINIO','fixture-bucket','fixture-snapshot-object',123,'READY',?,UTC_TIMESTAMP(6))`, digest[:])
	exec(t, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES
		(11,1,NULL,'',X'','DIR'),(21,2,NULL,'',X'','DIR'),(31,3,NULL,'',X'','DIR'),
		(12,1,11,'active-dir',CAST('active-dir' AS BINARY),'DIR')`)
	exec(t, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,revision) VALUES
		(8,1,12,'sample.txt',CAST('sample.txt' AS BINARY),'FILE','DOCUMENT',41,123,5)`)
	exec(t, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at,purge_started_at,purged_at) VALUES
		(13,1,11,'trashed',CAST('trashed' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6),NULL,NULL),
		(14,1,11,'purging',CAST('purging' AS BINARY),'DIR','PURGING',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6),NULL),
		(15,1,11,'purged',CAST('purged' AS BINARY),'DIR','PURGED',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6),UTC_TIMESTAMP(6))`)
	exec(t, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES
		(16,1,13,'hidden',CAST('hidden' AS BINARY),'DIR'),(17,1,14,'hidden',CAST('hidden' AS BINARY),'DIR'),
		(18,1,15,'hidden',CAST('hidden' AS BINARY),'DIR'),(32,3,31,'other-owner',CAST('other-owner' AS BINARY),'DIR')`)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	read := func(userID uint64) (domain.SyncSnapshotHeader, []domain.SyncSnapshotEntry, error) {
		var header domain.SyncSnapshotHeader
		var entries []domain.SyncSnapshotEntry
		err := store.WithUserSyncSnapshot(ctx, userID, func(readContext context.Context, reader *Store, snapshotHeader domain.SyncSnapshotHeader) error {
			header = snapshotHeader
			var readError error
			entries, readError = reader.ListSyncSnapshotEntries(readContext)
			return readError
		})
		return header, entries, err
	}
	want := []domain.SyncSnapshotEntry{
		{ID: 8, ParentID: 12, Name: "sample.txt", EntryType: "FILE", Revision: 5, FileSize: 123, VerifiedSHA256: &digest},
		{ID: 12, ParentID: 11, Name: "active-dir", EntryType: "DIR", Revision: 1},
	}

	// 2. 按数字 ID 返回完整活动条目，根目录单列，空树正常返回。
	t.Run("reachable entries and numeric order", func(t *testing.T) {
		header, entries, err := read(1)
		if err != nil || header.RootEntryID != 11 || header.HighWaterSequence != 20 || !reflect.DeepEqual(entries, want) {
			t.Fatalf("快照归属、可见性或字段错误，头部=%+v，条目=%+v，错误=%v", header, entries, err)
		}
		header, entries, err = read(2)
		if err != nil || header.RootEntryID != 21 || entries == nil || len(entries) != 0 {
			t.Fatalf("空树读取错误，头部=%+v，条目=%+v，错误=%v", header, entries, err)
		}
		if entries, err := store.ListSyncSnapshotEntries(ctx); entries != nil || err == nil {
			t.Fatal("未绑定快照的入口读取了条目")
		}
	})

	// 3. 在头部查询后，由另一个事务恢复一个分支并提高序号。
	t.Run("tree and sequence share the view across committed restore", func(t *testing.T) {
		err := store.WithUserSyncSnapshot(ctx, 1, func(readContext context.Context, reader *Store, header domain.SyncSnapshotHeader) error {
			writeContext, stop := context.WithTimeout(ctx, 2*time.Second)
			defer stop()
			writer, err := pool.BeginTx(writeContext, nil)
			if err != nil {
				return err
			}
			defer writer.Rollback()
			if _, err := writer.ExecContext(writeContext, "UPDATE file_entries SET status='ACTIVE',deleted_at=NULL,revision=2 WHERE id=13"); err != nil {
				return err
			}
			if _, err := writer.ExecContext(writeContext, "UPDATE users SET change_seq=21 WHERE id=1"); err != nil {
				return err
			}
			if err := writer.Commit(); err != nil {
				return err
			}
			entries, err := reader.ListSyncSnapshotEntries(readContext)
			if err != nil || header.HighWaterSequence != 20 || !reflect.DeepEqual(entries, want) {
				t.Fatalf("旧快照读到了恢复后的分支或新序号，头部=%+v，条目=%+v，错误=%v", header, entries, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		header, entries, err := read(1)
		if err != nil || header.HighWaterSequence != 21 || len(entries) != 4 || entries[2].ID != 13 || entries[3].ID != 16 {
			t.Fatalf("新快照未包括完整恢复分支，头部=%+v，条目=%+v，错误=%v", header, entries, err)
		}
	})

	// 4. 先接受 32 层和 10000 项边界，再验证多一层或多一项时整体失败。
	t.Run("maximum depth and one deeper", func(t *testing.T) {
		parentID := 11
		for index := 0; index < domain.MaxFileEntryDepth; index++ {
			entryID := 100 + index
			exec(t, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES (?,1,?,'nested',CAST('nested' AS BINARY),'DIR')", entryID, parentID)
			parentID = entryID
		}
		_, entries, err := read(1)
		if err != nil || len(entries) != domain.MaxFileEntryDepth+4 || entries[len(entries)-1].ID != uint64(parentID) {
			t.Fatalf("深度上限内的条目缺失，数量=%d，错误=%v", len(entries), err)
		}
		exec(t, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES (999,1,?,'too-deep',CAST('too-deep' AS BINARY),'DIR')", parentID)
		if _, entries, err := read(1); entries != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
			t.Fatalf("超深快照未整体失败，数量=%d，错误=%v", len(entries), err)
		}
	})
	t.Run("maximum count and one extra", func(t *testing.T) {
		for start := 1; start < domain.MaxSyncSnapshotEntries; start += 500 {
			end := min(start+500, domain.MaxSyncSnapshotEntries)
			values := make([]string, 0, end-start)
			args := make([]any, 0, (end-start)*3)
			for index := start; index < end; index++ {
				id := 1000 + index
				name := fmt.Sprintf("node-%d", id)
				values = append(values, "(?,3,32,?,?,'DIR')")
				args = append(args, id, name, []byte(name))
			}
			exec(t, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES "+strings.Join(values, ","), args...)
		}
		_, entries, err := read(3)
		if err != nil || len(entries) != domain.MaxSyncSnapshotEntries || entries[0].ID != 32 || entries[len(entries)-1].ID != 10999 {
			t.Fatalf("条目数边界读取错误，数量=%d，错误=%v", len(entries), err)
		}
		exec(t, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES (11000,3,31,'extra',CAST('extra' AS BINARY),'DIR')")
		if _, entries, err := read(3); entries != nil || !errors.Is(err, domain.ErrSyncSnapshotTooLarge) {
			t.Fatalf("超宽快照未整体失败，数量=%d，错误=%v", len(entries), err)
		}
	})
}
