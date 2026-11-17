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

func TestClaimUserUploadContentLeaseIntegration(t *testing.T) {
	// 1. 安装正式会话与 SINGLE 任务，所有资源归独占测试库管理。
	pool, _ := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	start := strings.Index(migrations.InitialSQL, "CREATE TABLE auth_sessions (")
	end := strings.Index(migrations.InitialSQL, "CREATE TABLE stored_objects (")
	if start < 0 || end <= start {
		t.Fatal("正式会话表定义未找到")
	}
	for _, statement := range []string{
		migrations.InitialSQL[start:end],
		"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(15,7,'98ee758a-0727-42de-ae7d-3bb33155e148','其他设备','WEB')",
		"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(17,7,11,UNHEX(REPEAT('11',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR),(18,7,15,UNHEX(REPEAT('22',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)",
		"UPDATE upload_tasks SET upload_mode='SINGLE',storage_upload_id=NULL,part_size=NULL,part_count=NULL,published_object_id=NULL,resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,status='UPLOADING',reserved_bytes=file_size,lease_owner=NULL,lease_until=NULL,lease_version=0,expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 DAY WHERE id=19",
		"INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,file_size,status,reserved_bytes,expires_at) VALUES(20,8,12,24,14,'foreign.bin','SINGLE',0,'UPLOADING',0,UTC_TIMESTAMP(6)+INTERVAL 1 DAY)",
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
	claim := func(current domain.Actor, taskID, objectID, version uint64, owner string) (*domain.UploadTask, error) {
		var task *domain.UploadTask
		err := store.WithUserTransaction(ctx, current, func(txContext context.Context, txStore *Store) error {
			var err error
			task, err = txStore.ClaimUserUploadContentLease(txContext, taskID, objectID, version, owner)
			return err
		})
		if err != nil {
			return nil, err
		}
		return task, nil
	}
	reset := func(t *testing.T) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET status='UPLOADING',lease_owner=NULL,lease_until=NULL,lease_version=0,expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 DAY WHERE id=19"); err != nil {
			t.Fatal(err)
		}
	}
	for _, mode := range []string{"initial", "released", "expired", "same account other device", "maximum next version"} {
		t.Run(mode, func(t *testing.T) {
			// 2. 核对数据库时间、精确六十秒期限和仅四项字段变化。
			reset(t)
			version := uint64(0)
			current := actor
			switch mode {
			case "released":
				version = 3
			case "expired":
				version = 3
				if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET lease_owner='old-worker',lease_until=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND WHERE id=19"); err != nil {
					t.Fatal(err)
				}
			case "same account other device":
				current = domain.Actor{UserID: 7, DeviceID: 15, SessionID: 18}
			case "maximum next version":
				version = ^uint64(0) - 1
			}
			if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET lease_version=? WHERE id=19", version); err != nil {
				t.Fatal(err)
			}
			before, err := store.FindUserUploadTask(ctx, 7, 19)
			if err != nil {
				t.Fatal(err)
			}
			profile, err := store.FindUserProfile(ctx, 7)
			if err != nil {
				t.Fatal(err)
			}
			var earliest, latest time.Time
			if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&earliest); err != nil {
				t.Fatal(err)
			}
			pool.SetMaxOpenConns(1)
			actual, err := claim(current, 19, 23, version, "new-worker")
			pool.SetMaxOpenConns(3)
			if err != nil {
				t.Fatal("单连接真实领取失败", err)
			}
			if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&latest); err != nil {
				t.Fatal(err)
			}
			if actual.LeaseOwner == nil || *actual.LeaseOwner != "new-worker" || actual.LeaseUntil == nil || actual.LeaseVersion != version+1 ||
				!actual.LeaseUntil.Equal(actual.UpdatedAt.Add(time.Minute)) || actual.UpdatedAt.Before(earliest) || actual.UpdatedAt.After(latest) || actual.UpdatedAt.Nanosecond()%1000 != 0 {
				t.Fatal("数据库租约期限、版本或时间错误")
			}
			expected := *before
			expected.LeaseOwner, expected.LeaseUntil, expected.LeaseVersion, expected.UpdatedAt = actual.LeaseOwner, actual.LeaseUntil, actual.LeaseVersion, actual.UpdatedAt
			persisted, err := store.FindUserUploadTask(ctx, 7, 19)
			if err != nil || !reflect.DeepEqual(actual, &expected) || !reflect.DeepEqual(persisted, actual) {
				t.Fatal("领取修改了额外任务事实或结果未提交", err)
			}
			afterProfile, err := store.FindUserProfile(ctx, 7)
			if err != nil || !reflect.DeepEqual(profile, afterProfile) {
				t.Fatal("租约领取改变了用户配额事实", err)
			}
		})
	}
	for _, fixture := range []struct {
		name, change string
		version      uint64
		wantError    error
	}{
		{"active", "UPDATE upload_tasks SET lease_owner='active-worker',lease_until=UTC_TIMESTAMP(6)+INTERVAL 1 MINUTE,lease_version=3 WHERE id=19", 3, domain.ErrUploadTaskBusy},
		{"exhausted", "UPDATE upload_tasks SET lease_version=18446744073709551615 WHERE id=19", ^uint64(0), domain.ErrUploadLeaseVersionExhausted},
		{"expired task", "UPDATE upload_tasks SET expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND WHERE id=19", 0, domain.ErrUploadTaskNotReady},
		{"wrong stage", "UPDATE upload_tasks SET status='VERIFYING' WHERE id=19", 0, domain.ErrUploadTaskNotReady},
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
			if task, err := claim(actor, 19, 23, fixture.version, "rejected-worker"); task != nil || !errors.Is(err, fixture.wantError) {
				t.Fatal("真实领取条件未拒绝", err)
			}
			after, err := store.FindUserUploadTask(ctx, 7, 19)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("拒绝领取后任务变化", err)
			}
		})
	}
	t.Run("scope and stale expected values", func(t *testing.T) {
		reset(t)
		for _, fixture := range []struct {
			taskID, objectID, version uint64
			wantError                 error
		}{{20, 24, 0, domain.ErrUploadTaskNotFound}, {999, 23, 0, domain.ErrUploadTaskNotFound}, {19, 24, 0, domain.ErrUploadTaskBusy}, {19, 23, 1, domain.ErrUploadTaskBusy}} {
			if task, err := claim(actor, fixture.taskID, fixture.objectID, fixture.version, "scope-worker"); task != nil || !errors.Is(err, fixture.wantError) {
				t.Fatal("真实任务范围或预期值未拒绝", err)
			}
		}
	})
	for _, mode := range []string{"callback failure", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			// 3. 领取后外层失败与取消，完整租约和时间随事务回滚。
			reset(t)
			before, err := store.FindUserUploadTask(ctx, 7, 19)
			if err != nil {
				t.Fatal(err)
			}
			failure := errors.New("outer lease failure")
			cancelled, stop := context.WithCancel(ctx)
			err = store.WithUserTransaction(cancelled, actor, func(txContext context.Context, txStore *Store) error {
				if _, err := txStore.ClaimUserUploadContentLease(txContext, 19, 23, 0, "rollback-worker"); err != nil {
					return err
				}
				if mode == "cancelled" {
					stop()
					return nil
				}
				return failure
			})
			stop()
			wantError := failure
			if mode == "cancelled" {
				wantError = context.Canceled
			}
			if !errors.Is(err, wantError) {
				t.Fatal("领取外层失败原因错误", err)
			}
			after, err := store.FindUserUploadTask(ctx, 7, 19)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("领取外层失败未完整回滚", err)
			}
		})
	}
	t.Run("concurrent requests one winner", func(t *testing.T) {
		reset(t)
		var workers sync.WaitGroup
		start := make(chan struct{})
		results := make(chan error, 2)
		for _, owner := range []string{"first-worker", "second-worker"} {
			workers.Add(1)
			go func(owner string) {
				defer workers.Done()
				<-start
				_, err := claim(actor, 19, 23, 0, owner)
				results <- err
			}(owner)
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
		if err != nil || successes != 1 || busy != 1 || task.LeaseVersion != 1 || task.LeaseOwner == nil {
			t.Fatal("并发领取未产生唯一租约", err)
		}
	})
	t.Run("maximum unsigned identifiers", func(t *testing.T) {
		maximum := ^uint64(0)
		for _, fixture := range []struct {
			statement string
			arguments []any
		}{
			{"INSERT INTO users(id,username,display_name,password_hash) VALUES(?,'claim_lease_max','完整用户','fixture_hash')", []any{maximum}},
			{"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(?,?,'a8ee758a-0727-42de-ae7d-3bb33155e148','完整设备','WEB')", []any{maximum, maximum}},
			{"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(?,?,?,UNHEX(REPEAT('44',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)", []any{maximum, maximum, maximum}},
			{"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(?,?,'MINIO','fixture-bucket','claim-lease-max',0)", []any{maximum, maximum}},
			{"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,?,NULL,'',X'','DIR')", []any{maximum, maximum}},
			{"INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,file_size,status,reserved_bytes,lease_version,expires_at) VALUES(?,?,?,?,?,'maximum.bin','SINGLE',0,'UPLOADING',0,?,UTC_TIMESTAMP(6)+INTERVAL 1 DAY)", []any{maximum, maximum, maximum, maximum, maximum, maximum - 1}},
		} {
			if _, err := pool.ExecContext(ctx, fixture.statement, fixture.arguments...); err != nil {
				t.Fatal(err)
			}
		}
		task, err := claim(domain.Actor{UserID: maximum, DeviceID: maximum, SessionID: maximum}, maximum, maximum, maximum-1, "maximum-worker")
		if err != nil || task.ID != maximum || task.UserID != maximum || task.DeviceID != maximum || task.StagedObjectID != maximum || task.TargetParentID != maximum || task.LeaseVersion != maximum {
			t.Fatal("完整无符号领取失败", err)
		}
	})
}
