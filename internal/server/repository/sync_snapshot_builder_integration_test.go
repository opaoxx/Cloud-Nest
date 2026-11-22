package repository

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestSyncSnapshotBuilderIntegration(t *testing.T) {
	// 1. 为两个账号建立独占夹具，第一棵树包含目录和已校验的零字节文件。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(2)
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	for _, statement := range []string{
		`INSERT INTO users(id,username,display_name,password_hash,change_seq) VALUES
		 (1,'snapshot_owner','Owner','fixture_hash',42),(2,'snapshot_empty','Empty','fixture_hash',0)`,
		`INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size,status,verified_sha256,verified_at) VALUES
		 (41,1,'MINIO','fixture-bucket','empty',0,'READY',UNHEX(SHA2('',256)),UTC_TIMESTAMP(6))`,
		`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES
		 (11,1,NULL,'',X'','DIR'),(12,1,11,'directory',CAST('directory' AS BINARY),'DIR'),(21,2,NULL,'',X'','DIR')`,
		`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES
		 (13,1,12,'original.txt',CAST('original.txt' AS BINARY),'FILE','DOCUMENT',41,0)`,
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("创建同步快照构建夹具失败", err)
		}
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	updateAfterHeader := true
	builder := service.NewSyncSnapshotBuilder(func(requestContext context.Context, userID uint64, read func(context.Context, service.SyncSnapshotReader, domain.SyncSnapshotHeader) error) error {
		return store.WithUserSyncSnapshot(requestContext, userID, func(readContext context.Context, reader *Store, header domain.SyncSnapshotHeader) error {
			// 2. 在首次构建读到头部后，从独立连接提交改名和序号更新。
			if updateAfterHeader {
				updateAfterHeader = false
				writeContext, stop := context.WithTimeout(requestContext, 2*time.Second)
				defer stop()
				writer, err := pool.BeginTx(writeContext, nil)
				if err != nil {
					return err
				}
				defer writer.Rollback()
				if _, err := writer.ExecContext(writeContext, "UPDATE file_entries SET name='renamed.txt',name_key=CAST('renamed.txt' AS BINARY),revision=2 WHERE id=13"); err != nil {
					return err
				}
				if _, err := writer.ExecContext(writeContext, "UPDATE users SET change_seq=43 WHERE id=1"); err != nil {
					return err
				}
				if err := writer.Commit(); err != nil {
					return err
				}
			}
			return read(readContext, reader, header)
		})
	}, func(entries []domain.SyncSnapshotEntry) (*domain.SyncSnapshotEntriesJSON, error) {
		if pool.Stats().InUse != 0 {
			t.Error("JSON 编码前应释放只读事务占用的连接")
		}
		return dto.EncodeSyncSnapshotEntries(entries)
	})

	// 3. 核对新旧结果的头部、条目顺序、字段和实际 JSON 容量。
	actor := domain.Actor{UserID: 1, DeviceID: 71, SessionID: 91}
	emptyDigest := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	for _, expected := range []struct {
		sequence       uint64
		name, revision string
	}{{42, "original.txt", "1"}, {43, "renamed.txt", "2"}} {
		built, err := builder.Build(ctx, actor)
		header := domain.SyncSnapshotHeader{RootEntryID: 11, HighWaterSequence: expected.sequence}
		if err != nil || built == nil || built.Header != header {
			t.Fatalf("构建头部不一致，数据=%+v，错误=%v", built, err)
		}
		body, err := json.Marshal(built.Entries.Items)
		if err != nil || len(body) != built.Entries.SizeBytes {
			t.Fatalf("实际编码大小不符，实际=%d，记录=%d，错误=%v", len(body), built.Entries.SizeBytes, err)
		}
		var entries []dto.SyncSnapshotEntry
		want := []dto.SyncSnapshotEntry{
			{EntryID: "12", ParentID: "11", Name: "directory", EntryType: "DIR", Revision: "1"},
			{EntryID: "13", ParentID: "12", Name: expected.name, EntryType: "FILE", Revision: expected.revision, SHA256: &emptyDigest},
		}
		if err := json.Unmarshal(body, &entries); err != nil || !reflect.DeepEqual(entries, want) {
			t.Fatalf("构建条目不一致，JSON=%s，错误=%v", body, err)
		}
	}
	emptyActor := domain.Actor{UserID: 2, DeviceID: 72, SessionID: 92}
	empty, err := builder.Build(ctx, emptyActor)
	if err != nil || empty == nil || empty.Header != (domain.SyncSnapshotHeader{RootEntryID: 21}) ||
		empty.Entries.Items == nil || len(empty.Entries.Items) != 0 || empty.Entries.SizeBytes != 2 {
		t.Fatalf("空树构建结果错误，数据=%+v，错误=%v", empty, err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE stored_objects SET status='VERIFYING' WHERE id=41"); err != nil {
		t.Fatal(err)
	}
	if result, err := builder.Build(ctx, actor); result != nil || !errors.Is(err, domain.ErrInvalidStoredObjectData) {
		t.Fatalf("对象异常的快照应整体拒绝，错误=%v", err)
	}
}
