package repository

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestUploadContentStateIntegration(t *testing.T) {
	// 1. 复用正式任务与关联记录夹具，串联范围读取、服务和接收状态判断。
	pool, want := uploadTaskDatabase(t)
	pool.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	tasks := service.NewUploadTaskService(store.FindUserUploadTask)
	actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
	now := want.ExpiresAt.Add(-time.Hour)
	check := func(t *testing.T, current domain.Actor, expected error) *domain.UploadTask {
		t.Helper()
		task, err := tasks.Get(ctx, current, 19)
		if err != nil {
			t.Fatal(err)
		}
		err = service.ValidateUploadContentState(task, current.UserID, 19, now)
		if !errors.Is(err, expected) {
			t.Fatal("实际任务接收状态判断错误", err)
		}
		again, err := tasks.Get(ctx, current, 19)
		if err != nil || !reflect.DeepEqual(task, again) {
			t.Fatal("状态校验改动了实际任务事实", err)
		}
		return task
	}
	t.Run("completed audit record", func(t *testing.T) { check(t, actor, domain.ErrUploadTaskNotReady) })
	if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET upload_mode='SINGLE',storage_upload_id=NULL,part_size=NULL,part_count=NULL,status='UPLOADING',published_object_id=NULL,resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,reserved_bytes=file_size WHERE id=19"); err != nil {
		t.Fatal(err)
	}
	for _, size := range []uint64{0, 2048, domain.MaxSingleUploadSizeBytes, domain.MaxSingleUploadSizeBytes + 1, ^uint64(0)} {
		t.Run("size "+strconv.FormatUint(size, 10), func(t *testing.T) {
			if _, err := pool.ExecContext(ctx, "UPDATE stored_objects SET file_size=? WHERE id=23", size); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET file_size=?,reserved_bytes=? WHERE id=19", size, size); err != nil {
				t.Fatal(err)
			}
			var expected error
			if size > domain.MaxSingleUploadSizeBytes {
				expected = domain.ErrUploadFileTooLarge
			}
			check(t, actor, expected)
		})
	}
	if _, err := pool.ExecContext(ctx, "UPDATE stored_objects SET file_size=2048 WHERE id=23"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET file_size=2048,reserved_bytes=2048 WHERE id=19"); err != nil {
		t.Fatal(err)
	}
	t.Run("NULL expected digest", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET expected_sha256=NULL WHERE id=19"); err != nil {
			t.Fatal(err)
		}
		if task := check(t, actor, nil); task.ExpectedSHA256 != nil {
			t.Fatal("接收阶段校验改变了可空摘要")
		}
	})
	t.Run("another device of same user", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(15,7,'58ee758a-0727-42de-ae7d-3bb33155e148','另一设备','ANDROID')"); err != nil {
			t.Fatal(err)
		}
		actual := check(t, domain.Actor{UserID: 7, DeviceID: 15, SessionID: 18}, nil)
		if actual.DeviceID != 11 {
			t.Fatal("状态校验改变了来源设备")
		}
	})
	t.Run("scope before readiness", func(t *testing.T) {
		if task, err := tasks.Get(ctx, domain.Actor{UserID: 8, DeviceID: 12, SessionID: 18}, 19); task != nil || !errors.Is(err, domain.ErrUploadTaskNotFound) {
			t.Fatal("范围读取未隐藏其他用户任务", err)
		}
		task, err := tasks.Get(ctx, actor, 19)
		if err != nil {
			t.Fatal(err)
		}
		if err := service.ValidateUploadContentState(task, 8, 19, now); !errors.Is(err, domain.ErrInvalidUploadTaskData) {
			t.Fatal("读回任务的错误归属未识别", err)
		}
	})
	for _, status := range []domain.UploadStatus{domain.UploadStatusCreating, domain.UploadStatusCompleting, domain.UploadStatusVerifying, domain.UploadStatusCompleted, domain.UploadStatusCancelling, domain.UploadStatusCancelled, domain.UploadStatusExpiring, domain.UploadStatusExpired, domain.UploadStatusFailed} {
		t.Run(string(status), func(t *testing.T) {
			reserved := uint64(2048)
			var published, entry, completed any
			if status.IsTerminal() {
				reserved = 0
			}
			if status == domain.UploadStatusCompleted {
				published, entry, completed = uint64(29), uint64(31), want.UpdatedAt
			}
			if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET status=?,reserved_bytes=?,published_object_id=?,result_entry_id=?,completed_at=? WHERE id=19", string(status), reserved, published, entry, completed); err != nil {
				t.Fatal(err)
			}
			check(t, actor, domain.ErrUploadTaskNotReady)
		})
	}
	if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET status='UPLOADING',reserved_bytes=file_size,published_object_id=NULL,result_entry_id=NULL,completed_at=NULL WHERE id=19"); err != nil {
		t.Fatal(err)
	}
	t.Run("expiry boundaries", func(t *testing.T) {
		for _, offset := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond} {
			var expected error
			if offset >= 0 {
				expected = domain.ErrUploadTaskNotReady
			}
			now = want.ExpiresAt.Add(offset)
			check(t, actor, expected)
		}
		now = want.ExpiresAt.Add(-time.Hour)
	})
	t.Run("partial reservation", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET reserved_bytes=2047 WHERE id=19"); err != nil {
			t.Fatal(err)
		}
		check(t, actor, domain.ErrInvalidUploadTaskData)
		if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET reserved_bytes=file_size WHERE id=19"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("published facts during receiving", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET published_object_id=29 WHERE id=19"); err != nil {
			t.Fatal(err)
		}
		check(t, actor, domain.ErrInvalidUploadTaskData)
		if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET published_object_id=NULL WHERE id=19"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("known multipart mode", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET upload_mode='MULTIPART',part_size=5242880,part_count=1,storage_upload_id='fixture-storage-session' WHERE id=19"); err != nil {
			t.Fatal(err)
		}
		check(t, actor, domain.ErrUploadTaskNotReady)
		if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET upload_mode='SINGLE',part_size=NULL,part_count=NULL,storage_upload_id=NULL WHERE id=19"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("lease facts retained", func(t *testing.T) {
		until := now.Add(time.Minute)
		if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET lease_owner='fixture-active-worker',lease_until=?,lease_version=42 WHERE id=19", until); err != nil {
			t.Fatal(err)
		}
		task := check(t, actor, nil)
		if task.LeaseOwner == nil || *task.LeaseOwner != "fixture-active-worker" || task.LeaseUntil == nil || !task.LeaseUntil.Equal(until) || task.LeaseVersion != 42 {
			t.Fatal("阶段校验修改了租约事实")
		}
	})
	t.Run("maximum identifiers", func(t *testing.T) {
		maximum := ^uint64(0)
		for _, statement := range []struct {
			sql  string
			args []any
		}{
			{"INSERT INTO users(id,username,display_name,password_hash) VALUES(?,'content_state_max','完整用户','fixture_hash')", []any{maximum}},
			{"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(?,?,'48ee758a-0727-42de-ae7d-3bb33155e148','完整设备','WINDOWS')", []any{maximum, maximum}},
			{"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(?,?,'MINIO','fixture-bucket','content-state-max',0)", []any{maximum, maximum}},
			{"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,?,NULL,'',X'','DIR')", []any{maximum, maximum}},
			{"INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,file_size,status,reserved_bytes,lease_version,expires_at) VALUES(?,?,?,?,?,'maximum.bin','SINGLE',0,'UPLOADING',0,?,?)", []any{maximum, maximum, maximum, maximum, maximum, maximum, want.ExpiresAt}},
		} {
			if _, err := pool.ExecContext(ctx, statement.sql, statement.args...); err != nil {
				t.Fatal(err)
			}
		}
		task, err := tasks.Get(ctx, domain.Actor{UserID: maximum, DeviceID: maximum, SessionID: maximum}, maximum)
		if err != nil {
			t.Fatal(err)
		}
		if err := service.ValidateUploadContentState(task, maximum, maximum, now); err != nil || task.LeaseVersion != maximum {
			t.Fatal("完整标识或阶段校验只读性错误", err)
		}
	})
}
