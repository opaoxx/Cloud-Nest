package repository

import (
	"context"
	"crypto/sha256"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestListSyncSnapshotVerifiedContentIntegration(t *testing.T) {
	// 1. 用正式表结构建立合成元数据，包含零字节文件、复用对象和隐藏的未就绪对象。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(2)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	exec := func(t *testing.T, statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal("准备对象校验夹具失败", err)
		}
	}
	emptyDigest := sha256.Sum256(nil)
	oldDigest := sha256.Sum256([]byte("old"))
	newDigest := sha256.Sum256([]byte("new!"))
	exec(t, `INSERT INTO users(id,username,display_name,password_hash,change_seq) VALUES
		(1,'snapshot_owner','Snapshot owner','fixture_hash',42),(2,'snapshot_other','Other owner','fixture_hash',0)`)
	exec(t, `INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size,status,verified_sha256,verified_at) VALUES
		(40,1,'MINIO','fixture-bucket','empty',0,'READY',?,UTC_TIMESTAMP(6)),
		(41,1,'MINIO','fixture-bucket','old',3,'READY',?,UTC_TIMESTAMP(6)),
		(42,1,'MINIO','fixture-bucket','new',4,'READY',?,UTC_TIMESTAMP(6)),
		(43,1,'MINIO','fixture-bucket','hidden',3,'STAGING',NULL,NULL),
		(44,2,'MINIO','fixture-bucket','other-owner',3,'READY',?,UTC_TIMESTAMP(6))`, emptyDigest[:], oldDigest[:], newDigest[:], oldDigest[:])
	exec(t, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES
		(11,1,NULL,'',X'','DIR'),(12,1,11,'directory',CAST('directory' AS BINARY),'DIR')`)
	exec(t, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at)
		VALUES (13,1,11,'trash',CAST('trash' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6))`)
	exec(t, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,revision) VALUES
		(20,1,11,'empty.txt',CAST('empty.txt' AS BINARY),'FILE','DOCUMENT',40,0,1),
		(21,1,12,'old.txt',CAST('old.txt' AS BINARY),'FILE','DOCUMENT',41,3,5),
		(22,1,12,'copy.txt',CAST('copy.txt' AS BINARY),'FILE','DOCUMENT',41,3,9),
		(23,1,13,'hidden.txt',CAST('hidden.txt' AS BINARY),'FILE','DOCUMENT',43,3,1)`)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	read := func() (domain.SyncSnapshotHeader, []domain.SyncSnapshotEntry, error) {
		var header domain.SyncSnapshotHeader
		var entries []domain.SyncSnapshotEntry
		err := store.WithUserSyncSnapshot(ctx, 1, func(readContext context.Context, reader *Store, snapshotHeader domain.SyncSnapshotHeader) error {
			header = snapshotHeader
			var readError error
			entries, readError = reader.ListSyncSnapshotEntries(readContext)
			return readError
		})
		return header, entries, err
	}
	want := []domain.SyncSnapshotEntry{
		{ID: 12, ParentID: 11, Name: "directory", EntryType: "DIR", Revision: 1},
		{ID: 20, ParentID: 11, Name: "empty.txt", EntryType: "FILE", Revision: 1, VerifiedSHA256: &emptyDigest},
		{ID: 21, ParentID: 12, Name: "old.txt", EntryType: "FILE", Revision: 5, FileSize: 3, VerifiedSHA256: &oldDigest},
		{ID: 22, ParentID: 12, Name: "copy.txt", EntryType: "FILE", Revision: 9, FileSize: 3, VerifiedSHA256: &oldDigest},
	}
	assertRejected := func(t *testing.T) {
		t.Helper()
		if _, entries, err := read(); entries != nil || !errors.Is(err, domain.ErrInvalidStoredObjectData) {
			t.Fatalf("异常可见对象应使整棵树失败，条目=%+v，错误=%v", entries, err)
		}
	}

	// 2. 核对每项摘要和大小，目录与零字节文件有不同的摘要语义。
	t.Run("directories empty files and shared verified content", func(t *testing.T) {
		header, entries, err := read()
		if err != nil || header.RootEntryID != 11 || header.HighWaterSequence != 42 || !reflect.DeepEqual(entries, want) {
			t.Fatalf("条目、大小或摘要不符，头部=%+v，条目=%+v，错误=%v", header, entries, err)
		}
		if entries[2].VerifiedSHA256 == entries[3].VerifiedSHA256 {
			t.Fatal("共享对象的两条结果须独立持有摘要")
		}
	})

	// 3. 在首个查询后提交内容替换，核对新旧视图各自返回配套的摘要、大小和版本。
	t.Run("committed replacement keeps content aligned with the sequence", func(t *testing.T) {
		err := store.WithUserSyncSnapshot(ctx, 1, func(readContext context.Context, reader *Store, header domain.SyncSnapshotHeader) error {
			writeContext, stop := context.WithTimeout(ctx, 2*time.Second)
			defer stop()
			writer, err := pool.BeginTx(writeContext, nil)
			if err != nil {
				return err
			}
			defer writer.Rollback()
			if _, err := writer.ExecContext(writeContext, "UPDATE file_entries SET object_id=42,file_size=4,revision=10 WHERE id=22"); err != nil {
				return err
			}
			if _, err := writer.ExecContext(writeContext, "UPDATE users SET change_seq=43 WHERE id=1"); err != nil {
				return err
			}
			if err := writer.Commit(); err != nil {
				return err
			}
			entries, err := reader.ListSyncSnapshotEntries(readContext)
			if err != nil || header.HighWaterSequence != 42 || !reflect.DeepEqual(entries, want) {
				t.Fatalf("旧视图混入了替换后的内容，头部=%+v，条目=%+v，错误=%v", header, entries, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		want[3].FileSize, want[3].Revision, want[3].VerifiedSHA256 = 4, 10, &newDigest
		header, entries, err := read()
		if err != nil || header.HighWaterSequence != 43 || !reflect.DeepEqual(entries, want) {
			t.Fatalf("新视图未取得已提交的新内容，头部=%+v，条目=%+v，错误=%v", header, entries, err)
		}
	})

	// 4. 注入可见对象异常。即使正常项排在前面，也不能交付部分结果。
	t.Run("non-ready object with a digest", func(t *testing.T) {
		exec(t, "UPDATE stored_objects SET status='VERIFYING' WHERE id=42")
		assertRejected(t)
		exec(t, "UPDATE stored_objects SET status='READY' WHERE id=42")
	})
	t.Run("size mismatch", func(t *testing.T) {
		exec(t, "UPDATE file_entries SET file_size=3 WHERE id=22")
		assertRejected(t)
		exec(t, "UPDATE file_entries SET file_size=4 WHERE id=22")
	})
	t.Run("missing or foreign object in a corrupt reference", func(t *testing.T) {
		// 仅在本测试库的一条专用连接上构造无法经由正常外键写入的异常引用。
		writer, err := pool.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer writer.Close()
		write := func(statement string, args ...any) {
			t.Helper()
			if _, err := writer.ExecContext(ctx, statement, args...); err != nil {
				t.Fatal("构造异常引用失败", err)
			}
		}
		write("SET SESSION FOREIGN_KEY_CHECKS=0")
		defer func() {
			if _, err := writer.ExecContext(ctx, "SET SESSION FOREIGN_KEY_CHECKS=1"); err != nil {
				t.Error("恢复测试连接外键校验失败", err)
			}
		}()
		for _, objectID := range []uint64{99, 44} {
			// 跨账号对象具备相同大小和有效摘要，只有正确核对归属才能拒绝。
			write("UPDATE file_entries SET object_id=?,file_size=3 WHERE id=22", objectID)
			assertRejected(t)
		}
		write("UPDATE file_entries SET object_id=42,file_size=4 WHERE id=22")
	})
}
