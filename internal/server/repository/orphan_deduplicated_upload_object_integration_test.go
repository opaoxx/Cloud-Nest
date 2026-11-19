package repository

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestMarkDeduplicatedUploadObjectOrphanedIntegration(t *testing.T) {
	pool, _ := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	execute := func(t *testing.T, statement string) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	execute(t, "INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(30,7,'MINIO','fixture-bucket','other-upload-staged',0)")
	execute(t, `INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,file_size,status,reserved_bytes,expires_at)
		VALUES(20,7,11,30,13,'other.txt','SINGLE',0,'UPLOADING',0,UTC_TIMESTAMP(6)+INTERVAL 1 DAY),
		      (21,8,12,24,14,'foreign.txt','SINGLE',0,'UPLOADING',0,UTC_TIMESTAMP(6)+INTERVAL 1 DAY)`)
	execute(t, `UPDATE upload_tasks SET upload_mode='SINGLE',part_size=NULL,part_count=NULL,storage_upload_id=NULL,lease_owner=NULL,lease_until=NULL WHERE id=19`)
	if object, err := store.MarkDeduplicatedUploadObjectOrphaned(ctx, 19); object != nil || err == nil {
		t.Fatal("事务外标记应被拒绝", object, err)
	}

	rollbackFixture := errors.New("fixture aborts after orphaning")
	for _, scenario := range []struct {
		name       string
		taskID     uint64
		changeSQL  string
		wantObject bool
		wantError  error
	}{
		{name: "reuse starts the timer", taskID: 19, wantObject: true},
		{name: "publication uses own staged object", taskID: 19, changeSQL: "UPDATE upload_tasks SET staged_object_id=29 WHERE id=19"},
		{name: "task still verifying", taskID: 19, changeSQL: "UPDATE upload_tasks SET status='VERIFYING' WHERE id=19", wantError: domain.ErrUploadTaskNotReady},
		{name: "another user task", taskID: 21, wantError: domain.ErrUploadTaskNotFound},
		{name: "object remains referenced by a trashed file", taskID: 19,
			changeSQL: "UPDATE file_entries SET object_id=23,status='TRASHED',deleted_at=UTC_TIMESTAMP(6) WHERE id=31", wantError: domain.ErrStoredObjectInUse},
		{name: "object remains referenced by unfinished task", taskID: 19,
			changeSQL: "UPDATE upload_tasks SET published_object_id=23 WHERE id=20", wantError: domain.ErrStoredObjectInUse},
		{name: "ready object protected", taskID: 19,
			changeSQL: "UPDATE stored_objects SET status='READY',verified_sha256=UNHEX(REPEAT('ab',32)),verified_at=UTC_TIMESTAMP(6) WHERE id=23",
			wantError: domain.ErrInvalidStoredObjectData},
		{name: "outer rollback restores timer and status", taskID: 19, wantObject: true, wantError: rollbackFixture},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 准备已完成任务及原始暂存对象快照。
			execute(t, `UPDATE upload_tasks SET staged_object_id=23 WHERE id=19`)
			execute(t, `UPDATE stored_objects SET status='VERIFYING',verified_sha256=NULL,verified_at=NULL,unreferenced_at=NULL,gc_after=NULL,
				storage_version_id='fixture-version',storage_etag='fixture-etag',updated_at='2026-11-17 08:00:00.123456' WHERE id=23`)
			before, err := store.FindUserStoredObject(ctx, 7, 23)
			if err != nil {
				t.Fatal(err)
			}
			published, err := store.FindUserStoredObject(ctx, 7, 29)
			if err != nil {
				t.Fatal(err)
			}

			// 2. 用当前用户事务标记并检查计时及重复调用。
			var result *domain.StoredObject
			err = store.withActiveUserTransaction(ctx, 7, func(txContext context.Context, txStore *Store) error {
				if scenario.changeSQL != "" {
					if err := txStore.database.WithContext(txContext).Exec(scenario.changeSQL).Error; err != nil {
						return err
					}
				}
				var err error
				result, err = txStore.MarkDeduplicatedUploadObjectOrphaned(txContext, scenario.taskID)
				if err != nil || !scenario.wantObject {
					return err
				}
				if result == nil || result.Status != domain.StoredObjectStatusOrphaned || result.UnreferencedAt == nil || result.GCAfter == nil ||
					!result.UpdatedAt.Equal(*result.UnreferencedAt) || !result.GCAfter.Equal(result.UnreferencedAt.Add(7*24*time.Hour)) {
					t.Fatal("未开始七天宽限期", result)
				}
				expected := *before
				expected.Status, expected.UnreferencedAt, expected.GCAfter, expected.UpdatedAt =
					domain.StoredObjectStatusOrphaned, result.UnreferencedAt, result.GCAfter, result.UpdatedAt
				if !reflect.DeepEqual(result, &expected) {
					t.Fatal("孤立标记影响了对象其他字段", result)
				}
				repeated, err := txStore.MarkDeduplicatedUploadObjectOrphaned(txContext, scenario.taskID)
				if err != nil || !reflect.DeepEqual(repeated, result) {
					t.Fatal("重复标记改变了原回收期限", repeated, err)
				}
				if scenario.wantError == rollbackFixture {
					return rollbackFixture
				}
				return nil
			})
			if !errors.Is(err, scenario.wantError) || (result != nil) != scenario.wantObject {
				t.Fatal("去重暂存对象标记错误", result, err)
			}

			// 3. 从独立连接核对提交或回滚，复用的 READY 对象保持原值。
			expected := before
			if scenario.wantObject && scenario.wantError == nil {
				expected = result
			}
			after, err := store.FindUserStoredObject(ctx, 7, 23)
			if err != nil || !reflect.DeepEqual(after, expected) {
				t.Fatal("暂存对象的持久结果错误", after, err)
			}
			afterPublished, err := store.FindUserStoredObject(ctx, 7, 29)
			if err != nil || !reflect.DeepEqual(afterPublished, published) {
				t.Fatal("已发布对象被改写", afterPublished, err)
			}
			if scenario.wantObject {
				task, err := store.FindUserUploadTask(ctx, 7, 19)
				if err != nil || task.Status != domain.UploadStatusCompleted || task.StagedObjectID != 23 ||
					task.PublishedObjectID == nil || *task.PublishedObjectID != 29 || task.ResultEntryID == nil || *task.ResultEntryID != 31 {
					t.Fatal("完成任务未保留原引用", task, err)
				}
			}
		})
	}
}
