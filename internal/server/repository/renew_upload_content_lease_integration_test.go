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

func TestRenewUserUploadContentLeaseIntegration(t *testing.T) {
	pool, _ := uploadTaskDatabase(t)
	pool.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := strings.Index(migrations.InitialSQL, "CREATE TABLE auth_sessions (")
	end := strings.Index(migrations.InitialSQL, "CREATE TABLE stored_objects (")
	if start < 0 || end <= start {
		t.Fatal("正式会话表定义未找到")
	}
	for _, statement := range []string{
		migrations.InitialSQL[start:end],
		"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(17,7,11,UNHEX(REPEAT('11',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)",
		"UPDATE upload_tasks SET status='UPLOADING',upload_mode='SINGLE',storage_upload_id=NULL,part_size=NULL,part_count=NULL,published_object_id=NULL,resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,reserved_bytes=file_size WHERE id=19",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
	rollback := errors.New("fixture outer rollback after renewal")
	for _, fixture := range []struct {
		name  string
		setup string
		want  error
	}{
		{"extend", "", nil},
		{"maximum version", "UPDATE upload_tasks SET lease_version=18446744073709551615 WHERE id=19", nil},
		{"longer deadline", "UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6)+INTERVAL 2 MINUTE WHERE id=19", nil},
		{"expired lease", "UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND WHERE id=19", domain.ErrUploadTaskBusy},
		{"expired task", "UPDATE upload_tasks SET expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND WHERE id=19", domain.ErrUploadTaskNotReady},
		{"wrong stage", "UPDATE upload_tasks SET status='VERIFYING',lease_owner=NULL,lease_until=NULL WHERE id=19", domain.ErrUploadTaskNotReady},
		{"old version", "", domain.ErrUploadTaskBusy},
		{"outer rollback", "", rollback},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 读取该场景的任务和对象快照。
			if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET status='UPLOADING',lease_owner='current-worker',lease_version=3,lease_until=UTC_TIMESTAMP(6)+INTERVAL 30 SECOND,expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 DAY WHERE id=19"); err != nil {
				t.Fatal(err)
			}
			if fixture.setup != "" {
				if _, err := pool.ExecContext(ctx, fixture.setup); err != nil {
					t.Fatal(err)
				}
			}
			before, err := store.FindUserUploadTask(ctx, actor.UserID, 19)
			if err != nil {
				t.Fatal(err)
			}
			beforeObject, err := store.FindUserStoredObject(ctx, actor.UserID, before.StagedObjectID)
			if err != nil {
				t.Fatal(err)
			}
			version := before.LeaseVersion
			if fixture.name == "old version" {
				version--
			}

			// 2. 单连接进入用户事务，确认真实写入或回滚。
			var result *domain.UploadTask
			err = store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				var err error
				result, err = txStore.RenewUserUploadContentLease(txContext, before.ID, before.StagedObjectID, version, "current-worker")
				if err != nil {
					return err
				}
				if fixture.name == "outer rollback" {
					return rollback
				}
				return nil
			})
			if !errors.Is(err, fixture.want) {
				t.Fatal("续租事务结果错误", err)
			}
			expected := *before
			if err == nil && fixture.name != "longer deadline" {
				if result == nil || result.LeaseUntil == nil || !result.LeaseUntil.After(*before.LeaseUntil) ||
					!result.LeaseUntil.Equal(result.UpdatedAt.Add(time.Minute)) {
					t.Fatal("续租没有用数据库时间延长六十秒", result)
				}
				expected.LeaseUntil, expected.UpdatedAt = result.LeaseUntil, result.UpdatedAt
			}
			if err == nil && !reflect.DeepEqual(result, &expected) {
				t.Fatal("事务内任务结果错误")
			}

			// 3. 在事务之外核对持久结果及暂存对象。
			after, err := store.FindUserUploadTask(ctx, actor.UserID, 19)
			if err != nil || !reflect.DeepEqual(after, &expected) {
				t.Fatal("持久任务与预期不一致", err)
			}
			afterObject, err := store.FindUserStoredObject(ctx, actor.UserID, before.StagedObjectID)
			if err != nil || !reflect.DeepEqual(beforeObject, afterObject) {
				t.Fatal("续租改变了对象记录", err)
			}
		})
	}
}
