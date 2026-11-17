package repository

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestReplaceUserUploadStagedObjectIntegration(t *testing.T) {
	// 1. 安装正式任务与会话，准备两个独立重试目标。
	pool, _ := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	start := strings.Index(migrations.InitialSQL, "CREATE TABLE auth_sessions (")
	end := strings.Index(migrations.InitialSQL, "CREATE TABLE stored_objects (")
	if start < 0 || end <= start {
		t.Fatal("正式会话表定义未找到")
	}
	for _, statement := range []string{
		migrations.InitialSQL[start:end],
		"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(15,7,'c8ee758a-0727-42de-ae7d-3bb33155e148','其他设备','WEB')",
		"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(17,7,11,UNHEX(REPEAT('11',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR),(18,7,15,UNHEX(REPEAT('22',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(25,7,'MINIO','fixture-bucket','retry-one',16777216),(26,7,'MINIO','fixture-bucket','retry-two',16777216)",
		"INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,file_size,status,reserved_bytes,expires_at) VALUES(21,8,12,24,14,'foreign.bin','SINGLE',0,'UPLOADING',0,UTC_TIMESTAMP(6)+INTERVAL 1 DAY)",
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
	reset := func(t *testing.T) {
		t.Helper()
		for _, statement := range []string{
			"UPDATE upload_tasks SET staged_object_id=23,upload_mode='SINGLE',storage_upload_id=NULL,part_size=NULL,part_count=NULL,published_object_id=NULL,resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,status='UPLOADING',file_size=16777216,reserved_bytes=16777216,lease_owner='current-worker',lease_until=UTC_TIMESTAMP(6)+INTERVAL 1 MINUTE,lease_version=3,expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 DAY WHERE id=19",
			"UPDATE stored_objects SET status='STAGING',file_size=16777216,storage_version_id=NULL,verified_sha256=NULL,storage_etag=NULL,verified_at=NULL,unreferenced_at=NULL,gc_after=NULL,last_error_code=NULL WHERE id IN (23,25,26)",
		} {
			if _, err := pool.ExecContext(ctx, statement); err != nil {
				t.Fatal(err)
			}
		}
	}
	replace := func(current domain.Actor, taskID, oldID, newID, version uint64, owner string) (*domain.UploadTask, error) {
		var task *domain.UploadTask
		err := store.WithUserTransaction(ctx, current, func(txContext context.Context, txStore *Store) error {
			var err error
			task, err = txStore.ReplaceUserUploadStagedObject(txContext, taskID, oldID, newID, version, owner)
			return err
		})
		if err != nil {
			return nil, err
		}
		return task, nil
	}
	for _, mode := range []string{"ordinary", "zero bytes", "maximum version", "other device", "old storage metadata"} {
		t.Run(mode, func(t *testing.T) {
			// 2. 单连接更新仅改变任务引用和更新时间，保留当前租约与对象事实。
			reset(t)
			version, current := uint64(3), actor
			switch mode {
			case "zero bytes":
				if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET file_size=0,reserved_bytes=0 WHERE id=19"); err != nil {
					t.Fatal(err)
				}
				if _, err := pool.ExecContext(ctx, "UPDATE stored_objects SET file_size=0 WHERE id IN (23,25)"); err != nil {
					t.Fatal(err)
				}
			case "maximum version":
				version = ^uint64(0)
				if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET lease_version=? WHERE id=19", version); err != nil {
					t.Fatal(err)
				}
			case "other device":
				current = domain.Actor{UserID: 7, DeviceID: 15, SessionID: 18}
			case "old storage metadata":
				if _, err := pool.ExecContext(ctx, "UPDATE stored_objects SET storage_version_id='old-version',storage_etag='old-etag' WHERE id=23"); err != nil {
					t.Fatal(err)
				}
			}
			beforeTask, err := store.FindUserUploadTask(ctx, 7, 19)
			if err != nil {
				t.Fatal(err)
			}
			beforeOld, err := store.FindUserStoredObject(ctx, 7, 23)
			if err != nil {
				t.Fatal(err)
			}
			beforeNew, err := store.FindUserStoredObject(ctx, 7, 25)
			if err != nil {
				t.Fatal(err)
			}
			var earliest, latest time.Time
			if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&earliest); err != nil {
				t.Fatal(err)
			}
			pool.SetMaxOpenConns(1)
			task, err := replace(current, 19, 23, 25, version, "current-worker")
			pool.SetMaxOpenConns(3)
			if err != nil {
				t.Fatal("真实单连接暂存对象替换失败", err)
			}
			if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&latest); err != nil {
				t.Fatal(err)
			}
			expected := *beforeTask
			expected.StagedObjectID, expected.UpdatedAt = 25, task.UpdatedAt
			if !reflect.DeepEqual(task, &expected) || task.UpdatedAt.Before(earliest) || task.UpdatedAt.After(latest) || task.UpdatedAt.Nanosecond()%1000 != 0 {
				t.Fatal("任务替换改变了额外字段或数据库时间错误")
			}
			afterOld, err := store.FindUserStoredObject(ctx, 7, 23)
			if err != nil {
				t.Fatal(err)
			}
			afterNew, err := store.FindUserStoredObject(ctx, 7, 25)
			if err != nil || !reflect.DeepEqual(beforeOld, afterOld) || !reflect.DeepEqual(beforeNew, afterNew) {
				t.Fatal("任务替换改变了对象事实", err)
			}
		})
	}
	for _, fixture := range []struct {
		name, change string
		wantError    error
	}{
		{"expired lease", "UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6) WHERE id=19", domain.ErrUploadTaskBusy},
		{"expired task", "UPDATE upload_tasks SET expires_at=UTC_TIMESTAMP(6) WHERE id=19", domain.ErrUploadTaskNotReady},
		{"wrong owner", "UPDATE upload_tasks SET lease_owner='other-worker' WHERE id=19", domain.ErrUploadTaskBusy},
		{"wrong version", "UPDATE upload_tasks SET lease_version=4 WHERE id=19", domain.ErrUploadTaskBusy},
		{"wrong stage", "UPDATE upload_tasks SET status='VERIFYING' WHERE id=19", domain.ErrUploadTaskNotReady},
		{"old orphaned", "UPDATE stored_objects SET status='ORPHANED' WHERE id=23", domain.ErrInvalidStoredObjectData},
		{"new wrong size", "UPDATE stored_objects SET file_size=1 WHERE id=25", domain.ErrInvalidStoredObjectData},
		{"new verifying", "UPDATE stored_objects SET status='VERIFYING' WHERE id=25", domain.ErrInvalidStoredObjectData},
		{"new written", "UPDATE stored_objects SET storage_etag='written-etag' WHERE id=25", domain.ErrInvalidStoredObjectData},
		{"new GC data", "UPDATE stored_objects SET unreferenced_at=UTC_TIMESTAMP(6),gc_after=UTC_TIMESTAMP(6)+INTERVAL 7 DAY WHERE id=25", domain.ErrInvalidStoredObjectData},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			reset(t)
			if _, err := pool.ExecContext(ctx, fixture.change); err != nil {
				t.Fatal(err)
			}
			before, err := store.FindUserUploadTask(ctx, 7, 19)
			if err != nil {
				t.Fatal(err)
			}
			if task, err := replace(actor, 19, 23, 25, 3, "current-worker"); task != nil || !errors.Is(err, fixture.wantError) {
				t.Fatal("真实任务或对象条件未拒绝", err)
			}
			after, err := store.FindUserUploadTask(ctx, 7, 19)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("失败替换改变了任务", err)
			}
		})
	}
	t.Run("scope and missing records", func(t *testing.T) {
		reset(t)
		for _, fixture := range []struct {
			taskID, oldID, newID uint64
			wantError            error
		}{{21, 24, 25, domain.ErrUploadTaskNotFound}, {999, 23, 25, domain.ErrUploadTaskNotFound}, {19, 26, 25, domain.ErrUploadTaskBusy}, {19, 23, 24, domain.ErrStoredObjectNotFound}, {19, 23, 999, domain.ErrStoredObjectNotFound}} {
			if task, err := replace(actor, fixture.taskID, fixture.oldID, fixture.newID, 3, "current-worker"); task != nil || !errors.Is(err, fixture.wantError) {
				t.Fatal("任务或新对象范围未隐藏", err)
			}
		}
	})
	for _, status := range []string{"UPLOADING", "FAILED"} {
		t.Run("target already assigned "+status, func(t *testing.T) {
			reset(t)
			if _, err := pool.ExecContext(ctx, "INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,file_size,status,reserved_bytes,expires_at) VALUES(20,7,11,25,13,'bound.bin','SINGLE',16777216,?,0,UTC_TIMESTAMP(6)+INTERVAL 1 DAY)", status); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := pool.ExecContext(ctx, "DELETE FROM upload_tasks WHERE user_id=7 AND id=20"); err != nil {
					t.Error(err)
				}
			}()
			if task, err := replace(actor, 19, 23, 25, 3, "current-worker"); task != nil || !errors.Is(err, domain.ErrUploadTaskStagedObjectConflict) {
				t.Fatal("目标对象历史引用未触发唯一键冲突", err)
			}
		})
	}
	t.Run("new object current read after snapshot", func(t *testing.T) {
		reset(t)
		err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			if _, err := txStore.FindUserStoredObject(txContext, 7, 25); err != nil {
				return err
			}
			if _, err := pool.ExecContext(ctx, "UPDATE stored_objects SET status='ORPHANED' WHERE id=25"); err != nil {
				return err
			}
			_, err := txStore.ReplaceUserUploadStagedObject(txContext, 19, 23, 25, 3, "current-worker")
			return err
		})
		if !errors.Is(err, domain.ErrInvalidStoredObjectData) {
			t.Fatal("旧快照掩盖了新对象当前状态", err)
		}
	})
	for _, mode := range []string{"commit", "after replace failure", "after orphan failure", "cancelled"} {
		t.Run("retry preparation "+mode, func(t *testing.T) {
			// 3. 真实租约、新对象、引用替换和旧对象孤立同事务提交或回滚。
			reset(t)
			if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET lease_owner=NULL,lease_until=NULL WHERE id=19"); err != nil {
				t.Fatal(err)
			}
			beforeTask, err := store.FindUserUploadTask(ctx, 7, 19)
			if err != nil {
				t.Fatal(err)
			}
			beforeOld, err := store.FindUserStoredObject(ctx, 7, 23)
			if err != nil {
				t.Fatal(err)
			}
			failure := errors.New("retry preparation failure")
			cancelled, stop := context.WithCancel(ctx)
			var newID uint64
			err = store.WithUserTransaction(cancelled, actor, func(txContext context.Context, txStore *Store) error {
				task, err := txStore.ClaimUserUploadContentLease(txContext, 19, 23, 3, "retry-worker")
				if err != nil {
					return err
				}
				object, err := txStore.CreateUserStagedObject(txContext, domain.CreateStagedObjectInput{Provider: "MINIO", Bucket: "fixture-bucket", ObjectKey: "retry/" + mode, SizeBytes: task.FileSize})
				if err != nil {
					return err
				}
				newID = object.ID
				if _, err := txStore.ReplaceUserUploadStagedObject(txContext, 19, 23, newID, task.LeaseVersion, "retry-worker"); err != nil {
					return err
				}
				if mode == "after replace failure" {
					return failure
				}
				if _, err := txStore.MarkUserStagedObjectOrphaned(txContext, 23); err != nil {
					return err
				}
				if mode == "after orphan failure" {
					return failure
				}
				if mode == "cancelled" {
					stop()
				}
				return nil
			})
			stop()
			afterTask, taskError := store.FindUserUploadTask(ctx, 7, 19)
			afterOld, oldError := store.FindUserStoredObject(ctx, 7, 23)
			afterNew, newError := store.FindUserStoredObject(ctx, 7, newID)
			if taskError != nil || oldError != nil {
				t.Fatal("读取准备结果失败", taskError, oldError)
			}
			if mode == "commit" {
				if err != nil || newError != nil || afterTask.StagedObjectID != newID || afterTask.LeaseVersion != 4 || afterOld.Status != domain.StoredObjectStatusOrphaned || afterNew.Status != domain.StoredObjectStatusStaging {
					t.Fatal("重试准备事实未共同提交", err, newError)
				}
				return
			}
			wantError := failure
			if mode == "cancelled" {
				wantError = context.Canceled
			}
			if !errors.Is(err, wantError) || !reflect.DeepEqual(beforeTask, afterTask) || !reflect.DeepEqual(beforeOld, afterOld) || afterNew != nil || !errors.Is(newError, domain.ErrStoredObjectNotFound) {
				t.Fatal("重试准备事实未共同回滚", err, newError)
			}
		})
	}
	t.Run("concurrent replacements one winner", func(t *testing.T) {
		reset(t)
		var workers sync.WaitGroup
		start := make(chan struct{})
		results := make(chan error, 2)
		for _, newID := range []uint64{25, 26} {
			workers.Add(1)
			go func(newID uint64) {
				defer workers.Done()
				<-start
				_, err := replace(actor, 19, 23, newID, 3, "current-worker")
				results <- err
			}(newID)
		}
		close(start)
		workers.Wait()
		close(results)
		successes, busy := 0, 0
		for err := range results {
			if err == nil {
				successes++
			} else if errors.Is(err, domain.ErrUploadTaskBusy) {
				busy++
			} else {
				t.Fatal(err)
			}
		}
		task, err := store.FindUserUploadTask(ctx, 7, 19)
		if err != nil || successes != 1 || busy != 1 || (task.StagedObjectID != 25 && task.StagedObjectID != 26) || task.LeaseVersion != 3 {
			t.Fatal("并发替换未产生唯一当前对象", err)
		}
	})
	t.Run("maximum unsigned identities and version", func(t *testing.T) {
		maximum := ^uint64(0)
		for _, fixture := range []struct {
			statement string
			arguments []any
		}{
			{"INSERT INTO users(id,username,display_name,password_hash) VALUES(?,'replace_object_max','完整用户','fixture_hash')", []any{maximum}},
			{"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(?,?,'d8ee758a-0727-42de-ae7d-3bb33155e148','完整设备','WEB')", []any{maximum, maximum}},
			{"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(?,?,?,UNHEX(REPEAT('44',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)", []any{maximum, maximum, maximum}},
			{"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(?,?,'MINIO','fixture-bucket','replace-max-old',0),(?,?,'MINIO','fixture-bucket','replace-max-new',0)", []any{maximum - 1, maximum, maximum, maximum}},
			{"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,?,NULL,'',X'','DIR')", []any{maximum, maximum}},
			{"INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,file_size,status,reserved_bytes,lease_owner,lease_until,lease_version,expires_at) VALUES(?,?,?,?,?,'maximum.bin','SINGLE',0,'UPLOADING',0,'current-worker',UTC_TIMESTAMP(6)+INTERVAL 1 MINUTE,?,UTC_TIMESTAMP(6)+INTERVAL 1 DAY)", []any{maximum, maximum, maximum, maximum - 1, maximum, maximum}},
		} {
			if _, err := pool.ExecContext(ctx, fixture.statement, fixture.arguments...); err != nil {
				t.Fatal(err)
			}
		}
		task, err := replace(domain.Actor{UserID: maximum, DeviceID: maximum, SessionID: maximum}, maximum, maximum-1, maximum, maximum, "current-worker")
		if err != nil || task.ID != maximum || task.UserID != maximum || task.StagedObjectID != maximum || task.LeaseVersion != maximum {
			t.Fatal("完整无符号暂存对象替换失败", err)
		}
	})
}
