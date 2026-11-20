package repository

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

type entryDownloadTargetChangingReader struct {
	*Store
	beforeContentRead func(context.Context) error
}

func (reader *entryDownloadTargetChangingReader) FindUserFileEntryContent(ctx context.Context, userID, entryID uint64) (*domain.FileEntryContent, error) {
	if err := reader.beforeContentRead(ctx); err != nil {
		return nil, err
	}
	return reader.Store.FindUserFileEntryContent(ctx, userID, entryID)
}

func TestEntryDownloadTargetServiceIntegration(t *testing.T) {
	// 1. 使用正式表建立多层目录、文件和属于同一用户的两个对象。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(2)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash) VALUES(1,'fixture_owner','文件所有者','fixture_hash'),(2,'fixture_other','其他用户','fixture_hash')",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','old-content',4),(42,1,'MINIO','fixture-bucket','new-content',8)",
		`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type)
		 VALUES(11,1,NULL,'',X'','DIR'),(22,2,NULL,'',X'','DIR'),
		       (12,1,11,'ancestor',CAST('ancestor' AS BINARY),'DIR'),(14,1,12,'parent',CAST('parent' AS BINARY),'DIR')`,
		`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,mime_type,revision)
		 VALUES(13,1,14,'old.png',CAST('old.png' AS BINARY),'FILE','IMAGE',41,4,'image/png',3)`,
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("创建下载目标夹具失败", err)
		}
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	var beforeContentRead func(context.Context) error
	targets := service.NewEntryDownloadTargetService(func(requestContext context.Context, action func(context.Context, service.EntryDownloadTargetReader) error) error {
		return store.WithFileEntrySnapshot(requestContext, func(snapshotContext context.Context, snapshot *Store) error {
			if beforeContentRead != nil {
				return action(snapshotContext, &entryDownloadTargetChangingReader{Store: snapshot, beforeContentRead: beforeContentRead})
			}
			return action(snapshotContext, snapshot)
		})
	})
	actor := domain.Actor{UserID: 1, DeviceID: 19, SessionID: 17}
	other := domain.Actor{UserID: 2, DeviceID: 29, SessionID: 27}
	assertTarget := func(t *testing.T, expected domain.EntryDownloadTarget) {
		t.Helper()
		target, err := targets.Get(ctx, actor, 13, expected.Revision)
		if err != nil || !reflect.DeepEqual(target, &expected) {
			t.Fatal("下载目标和已提交目录项事实不符", target, err)
		}
	}
	assertError := func(t *testing.T, identity domain.Actor, entryID, revision uint64, expected error) error {
		t.Helper()
		target, err := targets.Get(ctx, identity, entryID, revision)
		if target != nil || !errors.Is(err, expected) {
			t.Fatal("下载目标失败结果或错误类别不符", target, err)
		}
		return err
	}

	// 2. 核对已授权范围、目录类型及可见文件的版本冲突。
	t.Run("visible file and rejected targets", func(t *testing.T) {
		assertTarget(t, domain.EntryDownloadTarget{EntryID: 13, UserID: 1, Name: "old.png", Revision: 3, ObjectID: 41, SizeBytes: 4, MIMEType: new("image/png")})
		assertError(t, other, 13, 3, domain.ErrFileEntryNotFound)
		assertError(t, actor, 99, 3, domain.ErrFileEntryNotFound)
		assertError(t, actor, 14, 1, domain.ErrFileEntryNotFile)
		err := assertError(t, actor, 13, 2, domain.ErrEntryRevisionConflict)
		var conflict *domain.EntryRevisionConflictError
		if !errors.As(err, &conflict) || conflict.CurrentRevision != 3 {
			t.Fatal("版本冲突未携带当前版本", err)
		}
	})

	// 3. 在基础记录已读且内容引用尚未读取时，从另一连接提交对象替换。
	t.Run("version and content reference share the same snapshot", func(t *testing.T) {
		beforeContentRead = func(writeContext context.Context) error {
			_, err := pool.ExecContext(writeContext, "UPDATE file_entries SET object_id=42,file_size=8,mime_type=NULL,revision=4 WHERE id=13")
			return err
		}
		defer func() { beforeContentRead = nil }()
		assertTarget(t, domain.EntryDownloadTarget{EntryID: 13, UserID: 1, Name: "old.png", Revision: 3, ObjectID: 41, SizeBytes: 4, MIMEType: new("image/png")})
		beforeContentRead = nil
		assertTarget(t, domain.EntryDownloadTarget{EntryID: 13, UserID: 1, Name: "old.png", Revision: 4, ObjectID: 42, SizeBytes: 8})
	})

	// 4. 上层祖先被回收时，活动文件仍需通过整条路径检查。
	t.Run("hidden ancestor masks stale revision", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='TRASHED',deleted_at=UTC_TIMESTAMP(6) WHERE id=12"); err != nil {
			t.Fatal(err)
		}
		assertError(t, actor, 13, 3, domain.ErrFileEntryNotFound)
	})
}
