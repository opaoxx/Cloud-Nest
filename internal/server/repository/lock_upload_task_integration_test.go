package repository

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestLockUserUploadTaskIntegration(t *testing.T) {
	// 1. 复用正式任务夹具，补齐已认证事务需要的会话。
	pool, want := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	start := strings.Index(migrations.InitialSQL, "CREATE TABLE auth_sessions (")
	end := strings.Index(migrations.InitialSQL, "CREATE TABLE stored_objects (")
	if start < 0 || end <= start {
		t.Fatal("正式会话表定义未找到")
	}
	for _, statement := range []string{
		migrations.InitialSQL[start:end],
		"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(15,7,'58ee758a-0727-42de-ae7d-3bb33155e148','其他设备','WEB')",
		"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(17,7,11,UNHEX(REPEAT('11',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR),(18,7,15,UNHEX(REPEAT('22',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)",
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
	read := func(current domain.Actor, taskID uint64) (*domain.UploadTask, error) {
		var task *domain.UploadTask
		err := store.WithUserTransaction(ctx, current, func(txContext context.Context, txStore *Store) error {
			var err error
			task, err = txStore.LockUserUploadTask(txContext, taskID)
			return err
		})
		if err != nil {
			return nil, err
		}
		return task, nil
	}
	t.Run("complete record on one connection", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		actual, err := read(actor, 19)
		if err != nil || !reflect.DeepEqual(actual, &want) {
			t.Fatal("单连接锁定二十八项任务事实失败", err)
		}
		*actual.LeaseOwner, actual.ExpectedSHA256[0] = "changed", 0xff
		again, err := read(domain.Actor{UserID: 7, DeviceID: 15, SessionID: 18}, 19)
		if err != nil || !reflect.DeepEqual(again, &want) || again.DeviceID != 11 {
			t.Fatal("同账号跨设备或独立任务副本错误", err)
		}
	})
	t.Run("missing and foreign task", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,file_size,status,reserved_bytes,expires_at) VALUES(20,8,12,24,14,'foreign.bin','SINGLE',0,'UPLOADING',0,UTC_TIMESTAMP(6)+INTERVAL 1 DAY)"); err != nil {
			t.Fatal(err)
		}
		for _, taskID := range []uint64{20, 999} {
			if task, err := read(actor, taskID); task != nil || !errors.Is(err, domain.ErrUploadTaskNotFound) {
				t.Fatal("跨账号或缺失目标未隐藏", err)
			}
		}
	})
	for _, fixture := range []struct {
		name, change, restore string
		wantError             error
	}{
		{"disabled account", "UPDATE users SET status='DISABLED' WHERE id=7", "UPDATE users SET status='ACTIVE' WHERE id=7", domain.ErrAccountDisabled},
		{"revoked device", "UPDATE user_devices SET revoked_at=UTC_TIMESTAMP(6) WHERE id=11", "UPDATE user_devices SET revoked_at=NULL WHERE id=11", domain.ErrDeviceRevoked},
		{"revoked session", "UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=17", "UPDATE auth_sessions SET revoked_at=NULL WHERE id=17", domain.ErrSessionRevoked},
		{"expired session", "UPDATE auth_sessions SET expires_at='2000-01-01 00:00:00' WHERE id=17", "UPDATE auth_sessions SET expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR WHERE id=17", domain.ErrSessionExpired},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			if _, err := pool.ExecContext(ctx, fixture.change); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := pool.ExecContext(ctx, fixture.restore); err != nil {
					t.Error(err)
				}
			}()
			if task, err := read(actor, 19); task != nil || !errors.Is(err, fixture.wantError) {
				t.Fatal("当前认证状态未阻止任务锁读取", err)
			}
		})
	}

	// 2. 同一可重复读事务先建立旧快照，再核对锁定读取取得最新提交值。
	t.Run("current read after existing snapshot", func(t *testing.T) {
		err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			before, err := txStore.FindUserUploadTask(txContext, 7, 19)
			if err != nil {
				return err
			}
			if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET last_error_message='committed after snapshot',lease_version=lease_version+1 WHERE id=19"); err != nil {
				return err
			}
			locked, err := txStore.LockUserUploadTask(txContext, 19)
			if err != nil {
				return err
			}
			if *locked.LastErrorMessage != "committed after snapshot" || locked.LeaseVersion != before.LeaseVersion+1 {
				return errors.New("任务当前读未取得最新已提交事实")
			}
			ordinary, err := txStore.FindUserUploadTask(txContext, 7, 19)
			if err != nil || !reflect.DeepEqual(ordinary, before) {
				return errors.New("任务锁查询影响了普通快照读取")
			}
			again, err := txStore.LockUserUploadTask(txContext, 19)
			if err != nil || !reflect.DeepEqual(again, locked) {
				return errors.New("重复锁定当前读失败")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
	for _, mode := range []string{"commit", "rollback"} {
		t.Run("lock held until "+mode, func(t *testing.T) {
			// 3. 外部更新等待行锁，外层事务结束后继续完成。
			failure := errors.New("outer task lock rollback")
			started := make(chan struct{})
			finished := make(chan error, 1)
			workerStarted := false
			err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				if _, err := txStore.LockUserUploadTask(txContext, 19); err != nil {
					return err
				}
				workerStarted = true
				go func() {
					close(started)
					_, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET last_error_message=? WHERE id=19", mode)
					finished <- err
				}()
				<-started
				select {
				case err := <-finished:
					finished <- err
					return errors.New("外部更新提前越过任务行锁")
				case <-time.After(100 * time.Millisecond):
				}
				if mode == "rollback" {
					return failure
				}
				return nil
			})
			if workerStarted {
				select {
				case workerError := <-finished:
					if workerError != nil {
						t.Error("事务结束后任务锁未释放", workerError)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("等待任务锁释放超时")
				}
			}
			if (mode == "commit" && err != nil) || (mode == "rollback" && !errors.Is(err, failure)) {
				t.Fatal("外层事务结束结果错误", err)
			}
			task, err := read(actor, 19)
			if err != nil || task.LastErrorMessage == nil || *task.LastErrorMessage != mode {
				t.Fatal("等待行锁的更新未实际提交", err)
			}
		})
	}
	t.Run("lock waiting cancellation", func(t *testing.T) {
		writer, err := pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer writer.Rollback()
		if _, err := writer.ExecContext(ctx, "UPDATE upload_tasks SET last_error_message='uncommitted lock' WHERE id=19"); err != nil {
			t.Fatal(err)
		}
		err = store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			waiting, stop := context.WithTimeout(txContext, 100*time.Millisecond)
			defer stop()
			task, err := txStore.LockUserUploadTask(waiting, 19)
			if task != nil {
				t.Error("任务锁等待取消仍交付结果")
			}
			return err
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("任务行锁等待未服从请求期限", err)
		}
		if err := writer.Rollback(); err != nil {
			t.Fatal(err)
		}
		if _, err := read(actor, 19); err != nil {
			t.Fatal("等待取消后连接池不能继续使用", err)
		}
	})
	t.Run("locked SINGLE state validation", func(t *testing.T) {
		for _, statement := range []string{
			"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(25,7,'MINIO','fixture-bucket','locked-single',0)",
			"INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,file_size,status,reserved_bytes,expires_at) VALUES(21,7,11,25,13,'empty.bin','SINGLE',0,'UPLOADING',0,UTC_TIMESTAMP(6)+INTERVAL 1 DAY)",
		} {
			if _, err := pool.ExecContext(ctx, statement); err != nil {
				t.Fatal(err)
			}
		}
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		if err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			task, err := txStore.LockUserUploadTask(txContext, 21)
			if err != nil {
				return err
			}
			return service.ValidateUploadContentState(task, actor.UserID, 21, time.Now())
		}); err != nil {
			t.Fatal("单连接锁定任务与接收阶段判断组合失败", err)
		}
	})
	t.Run("maximum unsigned identifiers", func(t *testing.T) {
		maximum := ^uint64(0)
		for _, fixture := range []struct {
			statement string
			arguments []any
		}{
			{"INSERT INTO users(id,username,display_name,password_hash) VALUES(?,'lock_task_max','完整用户','fixture_hash')", []any{maximum}},
			{"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(?,?,'68ee758a-0727-42de-ae7d-3bb33155e148','完整设备','WEB')", []any{maximum, maximum}},
			{"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(?,?,?,UNHEX(REPEAT('44',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)", []any{maximum, maximum, maximum}},
			{"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(?,?,'MINIO','fixture-bucket','lock-task-max',0)", []any{maximum, maximum}},
			{"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,?,NULL,'',X'','DIR')", []any{maximum, maximum}},
			{"INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,file_size,status,reserved_bytes,lease_version,expires_at) VALUES(?,?,?,?,?,'maximum.bin','SINGLE',0,'UPLOADING',0,?,UTC_TIMESTAMP(6)+INTERVAL 1 DAY)", []any{maximum, maximum, maximum, maximum, maximum, maximum}},
		} {
			if _, err := pool.ExecContext(ctx, fixture.statement, fixture.arguments...); err != nil {
				t.Fatal(err)
			}
		}
		task, err := read(domain.Actor{UserID: maximum, DeviceID: maximum, SessionID: maximum}, maximum)
		if err != nil || task.ID != maximum || task.UserID != maximum || task.DeviceID != maximum || task.StagedObjectID != maximum || task.TargetParentID != maximum || task.LeaseVersion != maximum {
			t.Fatal("完整无符号任务事实错误", err)
		}
	})
}
