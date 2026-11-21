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

func TestRenewUserUploadPartExpiryIntegration(t *testing.T) {
	pool, _ := uploadTaskDatabase(t)
	pool.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	execute := func(t *testing.T, statement string, arguments ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
			t.Fatal(err)
		}
	}

	// 1. 在专用库补齐两个账号及同账号跨设备的认证会话。
	start := strings.Index(migrations.InitialSQL, "CREATE TABLE auth_sessions (")
	end := strings.Index(migrations.InitialSQL, "CREATE TABLE stored_objects (")
	if start < 0 || end <= start {
		t.Fatal("正式会话表定义未找到")
	}
	execute(t, migrations.InitialSQL[start:end])
	execute(t, "INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(15,7,'58ee758a-0727-42de-ae7d-3bb33155e148','other-device','WEB')")
	execute(t, "UPDATE users SET status='ACTIVE' WHERE id=8")
	execute(t, `INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES
		(17,7,11,UNHEX(REPEAT('11',32)),UTC_TIMESTAMP(6)+INTERVAL 1 DAY),
		(18,7,15,UNHEX(REPEAT('22',32)),UTC_TIMESTAMP(6)+INTERVAL 1 DAY),
		(22,8,12,UNHEX(REPEAT('33',32)),UTC_TIMESTAMP(6)+INTERVAL 1 DAY)`)
	const userID, taskID, size = 7, 19, 16777217
	creator := domain.Actor{UserID: userID, DeviceID: 11, SessionID: 17}
	otherDevice := domain.Actor{UserID: userID, DeviceID: 15, SessionID: 18}
	foreign := domain.Actor{UserID: 8, DeviceID: 12, SessionID: 22}
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	oldExpiry := now.Add(time.Hour)
	execute(t, "SET timestamp = ?", now.Unix())
	execute(t, "UPDATE stored_objects SET file_size=? WHERE id=23", size)
	reset := func(t *testing.T, expiry time.Time) {
		t.Helper()
		execute(t, "UPDATE users SET storage_quota=?,used_storage=1,reserved_storage=? WHERE id=7", size+1, size)
		execute(t, `UPDATE upload_tasks SET status='UPLOADING',storage_upload_id='fixture+A/=',published_object_id=NULL,
			resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,file_size=?,part_size=16777216,part_count=2,reserved_bytes=?,
			lease_owner=NULL,lease_until=NULL,lease_version=4,next_attempt_at=NULL,last_error_code=NULL,last_error_message=NULL,
			expires_at=?,updated_at=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND WHERE id=19`, size, size, expiry)
	}
	type state struct {
		task  *domain.UploadTask
		quota [3]uint64
	}
	read := func(t *testing.T) state {
		t.Helper()
		task, err := store.FindUserUploadTask(ctx, userID, taskID)
		if err != nil {
			t.Fatal(err)
		}
		current := state{task: task}
		if err := pool.QueryRowContext(ctx, "SELECT storage_quota,used_storage,reserved_storage FROM users WHERE id=7").
			Scan(&current.quota[0], &current.quota[1], &current.quota[2]); err != nil {
			t.Fatal(err)
		}
		return current
	}
	renew := func(actor domain.Actor, expected time.Time) (*domain.UploadTask, error) {
		return store.RenewUserUploadPartExpiry(ctx, actor, taskID, expected, service.ResolveUploadPartRenewalExpiry)
	}
	checkRejected := func(t *testing.T, actor domain.Actor, expected time.Time, want error) {
		t.Helper()
		before := read(t)
		if result, err := renew(actor, expected); result != nil || !errors.Is(err, want) {
			t.Fatal("续期未按预期拒绝", err, want)
		}
		if !reflect.DeepEqual(before, read(t)) {
			t.Fatal("失败的续期修改了任务或账号用量")
		}
	}

	// 2. 同账号另一台设备续期，复核只有期限和更新时间变化。
	t.Run("renew and reject stale deadline", func(t *testing.T) {
		reset(t, oldExpiry)
		before := read(t)
		result, err := renew(otherDevice, oldExpiry)
		if err != nil || result == nil {
			t.Fatal("同账号另一台设备续期失败", err)
		}
		before.task.ExpiresAt, before.task.UpdatedAt = now.Add(24*time.Hour), now
		if !reflect.DeepEqual(result, before.task) || !reflect.DeepEqual(before, read(t)) {
			t.Fatal("事务未按数据库时间续期，或修改了其他字段")
		}
		checkRejected(t, creator, oldExpiry, domain.ErrUploadTaskNotReady)
	})
	t.Run("keep later deadline without updating", func(t *testing.T) {
		later := now.Add(25 * time.Hour)
		reset(t, later)
		before := read(t)
		for range 2 {
			result, err := renew(creator, later)
			if err != nil || !reflect.DeepEqual(before.task, result) || !reflect.DeepEqual(before, read(t)) {
				t.Fatal("保持原期限时不能产生写入", err)
			}
		}
	})
	t.Run("check ownership and live credentials", func(t *testing.T) {
		reset(t, oldExpiry)
		checkRejected(t, foreign, oldExpiry, domain.ErrUploadTaskNotFound)
		execute(t, "UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=17")
		checkRejected(t, creator, oldExpiry, domain.ErrSessionRevoked)
		execute(t, "UPDATE auth_sessions SET revoked_at=NULL WHERE id=17")
	})
	t.Run("check quota lease and readback", func(t *testing.T) {
		reset(t, oldExpiry)
		execute(t, "UPDATE upload_tasks SET lease_owner='fixture-worker',lease_until=UTC_TIMESTAMP(6)+INTERVAL 1 SECOND WHERE id=19")
		checkRejected(t, creator, oldExpiry, domain.ErrUploadTaskBusy)
		execute(t, "UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6) WHERE id=19")
		execute(t, "UPDATE users SET reserved_storage=reserved_storage-1 WHERE id=7")
		checkRejected(t, creator, oldExpiry, domain.ErrUploadTaskNotReady)
		execute(t, "UPDATE users SET reserved_storage=reserved_storage+1 WHERE id=7")
		execute(t, "CREATE TRIGGER fixture_invalid_renewal BEFORE UPDATE ON upload_tasks FOR EACH ROW SET NEW.lease_version=OLD.lease_version+1")
		defer execute(t, "DROP TRIGGER fixture_invalid_renewal")
		checkRejected(t, creator, oldExpiry, domain.ErrInvalidUploadTaskData)
	})

	// 3. 两条事务并发使用同一旧期限，只有一个能改变任务。
	t.Run("concurrent renewal uses old deadline once", func(t *testing.T) {
		execute(t, "SET timestamp = DEFAULT")
		pool.SetMaxOpenConns(3)
		expiry := time.Now().UTC().Truncate(time.Microsecond).Add(time.Hour)
		reset(t, expiry)
		before := read(t)
		type outcome struct {
			task *domain.UploadTask
			err  error
		}
		results := make(chan outcome, 2)
		start := make(chan struct{})
		for _, actor := range []domain.Actor{creator, otherDevice} {
			go func() {
				<-start
				task, err := renew(actor, expiry)
				results <- outcome{task, err}
			}()
		}
		close(start)
		var winner *domain.UploadTask
		rejected := 0
		for range 2 {
			result := <-results
			switch {
			case result.err == nil && result.task != nil:
				winner = result.task
			case result.task == nil && errors.Is(result.err, domain.ErrUploadTaskNotReady):
				rejected++
			default:
				t.Fatal("并发续期结果异常", result.err)
			}
		}
		after := read(t)
		if rejected != 1 || winner == nil || !winner.ExpiresAt.After(expiry) || !reflect.DeepEqual(after.task, winner) || before.quota != after.quota {
			t.Fatal("同一旧期限的并发请求没有恰好一次写入，或改变了账号用量")
		}
	})
}
