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

func TestUploadContentLeaseIntegration(t *testing.T) {
	// 1. 安装真实任务和会话，在单连接中串联认证、当前读及租约判断。
	pool, fixture := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	start := strings.Index(migrations.InitialSQL, "CREATE TABLE auth_sessions (")
	end := strings.Index(migrations.InitialSQL, "CREATE TABLE stored_objects (")
	if start < 0 || end <= start {
		t.Fatal("正式会话表定义未找到")
	}
	for _, statement := range []string{
		migrations.InitialSQL[start:end],
		"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(15,7,'78ee758a-0727-42de-ae7d-3bb33155e148','其他设备','WEB')",
		"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(17,7,11,UNHEX(REPEAT('11',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR),(18,7,15,UNHEX(REPEAT('22',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	pool.SetMaxOpenConns(1)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
	now := fixture.ExpiresAt.Add(-time.Hour)
	check := func(t *testing.T, current domain.Actor, taskID uint64, at time.Time, wantError error) {
		t.Helper()
		before, err := store.FindUserUploadTask(ctx, current.UserID, taskID)
		if err != nil {
			t.Fatal(err)
		}
		err = store.WithUserTransaction(ctx, current, func(txContext context.Context, txStore *Store) error {
			task, err := txStore.LockUserUploadTask(txContext, taskID)
			if err != nil {
				return err
			}
			return service.ValidateUploadContentLease(task, current.UserID, taskID, at)
		})
		if !errors.Is(err, wantError) {
			t.Fatal("真实任务接收租约判断错误", err)
		}
		after, err := store.FindUserUploadTask(ctx, current.UserID, taskID)
		if err != nil || !reflect.DeepEqual(after, before) {
			t.Fatal("租约判断修改了真实任务事实", err)
		}
	}
	t.Run("retained completed task", func(t *testing.T) {
		check(t, actor, 19, now, domain.ErrUploadTaskNotReady)
	})
	if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET upload_mode='SINGLE',storage_upload_id=NULL,part_size=NULL,part_count=NULL,published_object_id=NULL,resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,status='UPLOADING',reserved_bytes=file_size,lease_owner=NULL,lease_until=NULL,lease_version=0 WHERE id=19"); err != nil {
		t.Fatal(err)
	}

	// 2. 真实可空租约、期限边界和完整无符号版本逐项核对。
	for _, mode := range []string{"initial", "released", "expired", "exact expiry", "one nanosecond before", "one nanosecond after", "active", "maximum next version", "unleased maximum", "expired maximum", "busy maximum", "zero claimed version", "empty owner", "owner length limit", "timezone"} {
		t.Run(mode, func(t *testing.T) {
			owner, until, version := "fixture-worker", now.Add(-time.Minute), uint64(42)
			var leaseOwner, leaseUntil any = owner, until
			at := now
			var wantError error
			switch mode {
			case "initial":
				leaseOwner, leaseUntil, version = nil, nil, 0
			case "released":
				leaseOwner, leaseUntil = nil, nil
			case "exact expiry":
				leaseUntil = now
			case "one nanosecond before":
				leaseUntil, at, wantError = now, now.Add(-time.Nanosecond), domain.ErrUploadTaskBusy
			case "one nanosecond after":
				leaseUntil, at = now, now.Add(time.Nanosecond)
			case "active":
				leaseUntil, wantError = now.Add(time.Minute), domain.ErrUploadTaskBusy
			case "maximum next version":
				version = ^uint64(0) - 1
			case "unleased maximum":
				leaseOwner, leaseUntil, version, wantError = nil, nil, ^uint64(0), domain.ErrUploadLeaseVersionExhausted
			case "expired maximum":
				version, wantError = ^uint64(0), domain.ErrUploadLeaseVersionExhausted
			case "busy maximum":
				leaseUntil, version, wantError = now.Add(time.Minute), ^uint64(0), domain.ErrUploadTaskBusy
			case "zero claimed version":
				version, wantError = 0, domain.ErrInvalidUploadTaskData
			case "empty owner":
				leaseOwner, wantError = "", domain.ErrInvalidUploadTaskData
			case "owner length limit":
				leaseOwner = strings.Repeat("a", 64)
			case "timezone":
				at = now.In(time.FixedZone("fixture UTC+8", 8*3600))
			}
			if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET lease_owner=?,lease_until=?,lease_version=? WHERE id=19", leaseOwner, leaseUntil, version); err != nil {
				t.Fatal(err)
			}
			check(t, actor, 19, at, wantError)
		})
	}
	t.Run("same account other device", func(t *testing.T) {
		check(t, domain.Actor{UserID: 7, DeviceID: 15, SessionID: 18}, 19, now, nil)
	})
	t.Run("task expiry precedes active lease", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET lease_owner='fixture-worker',lease_until=?,lease_version=42 WHERE id=19", fixture.ExpiresAt.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		check(t, actor, 19, fixture.ExpiresAt, domain.ErrUploadTaskNotReady)
	})
	t.Run("latest active lease after old snapshot", func(t *testing.T) {
		// 3. 已有快照中的旧租约过期，当前读必须识别刚提交的有效租约。
		pool.SetMaxOpenConns(3)
		defer pool.SetMaxOpenConns(1)
		if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET lease_until=?,lease_version=42 WHERE id=19", now.Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
		err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			old, err := txStore.FindUserUploadTask(txContext, 7, 19)
			if err != nil {
				return err
			}
			if err := service.ValidateUploadContentLease(old, 7, 19, now); err != nil {
				return err
			}
			if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET lease_until=?,lease_version=43 WHERE id=19", now.Add(time.Minute)); err != nil {
				return err
			}
			current, err := txStore.LockUserUploadTask(txContext, 19)
			if err != nil {
				return err
			}
			if current.LeaseVersion != 43 {
				return errors.New("任务当前读保留了旧租约版本")
			}
			return service.ValidateUploadContentLease(current, 7, 19, now)
		})
		if !errors.Is(err, domain.ErrUploadTaskBusy) {
			t.Fatal("新提交的有效租约未阻止领取", err)
		}
		current, err := store.FindUserUploadTask(ctx, 7, 19)
		if err != nil || current.LeaseVersion != 43 {
			t.Fatal("拒绝领取改变了其他事务的已提交事实", err)
		}
	})
	t.Run("maximum identifiers with exhausted version", func(t *testing.T) {
		maximum := ^uint64(0)
		for _, record := range []struct {
			statement string
			arguments []any
		}{
			{"INSERT INTO users(id,username,display_name,password_hash) VALUES(?,'lease_guard_max','完整用户','fixture_hash')", []any{maximum}},
			{"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(?,?,'88ee758a-0727-42de-ae7d-3bb33155e148','完整设备','WEB')", []any{maximum, maximum}},
			{"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(?,?,?,UNHEX(REPEAT('44',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)", []any{maximum, maximum, maximum}},
			{"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(?,?,'MINIO','fixture-bucket','lease-guard-max',0)", []any{maximum, maximum}},
			{"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,?,NULL,'',X'','DIR')", []any{maximum, maximum}},
			{"INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,file_size,status,reserved_bytes,lease_version,expires_at) VALUES(?,?,?,?,?,'maximum.bin','SINGLE',0,'UPLOADING',0,?,?)", []any{maximum, maximum, maximum, maximum, maximum, maximum, fixture.ExpiresAt}},
		} {
			if _, err := pool.ExecContext(ctx, record.statement, record.arguments...); err != nil {
				t.Fatal(err)
			}
		}
		check(t, domain.Actor{UserID: maximum, DeviceID: maximum, SessionID: maximum}, maximum, now, domain.ErrUploadLeaseVersionExhausted)
	})
}
