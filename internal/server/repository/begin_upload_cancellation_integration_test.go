package repository

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestBeginUserUploadCancellationIntegration(t *testing.T) {
	// 1. 在独占库建立已认证会话和可取消的单文件任务。
	pool, fixture := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
	defer cancel()
	execute := func(t *testing.T, statement string, arguments ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
			t.Fatal(err)
		}
	}
	start := strings.Index(migrations.InitialSQL, "CREATE TABLE auth_sessions (")
	end := strings.Index(migrations.InitialSQL, "CREATE TABLE stored_objects (")
	if start < 0 || end <= start {
		t.Fatal("正式会话表定义未找到")
	}
	for _, statement := range []string{
		migrations.InitialSQL[start:end],
		"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(15,7,'78ee758a-0727-42de-ae7d-3bb33155e148','同账号另一设备','WEB')",
		"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(17,7,11,UNHEX(REPEAT('11',32)),UTC_TIMESTAMP(6)+INTERVAL 1 DAY),(18,7,15,UNHEX(REPEAT('22',32)),UTC_TIMESTAMP(6)+INTERVAL 1 DAY)",
		"INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,file_size,status,reserved_bytes,expires_at) VALUES(20,8,12,24,14,'foreign.bin','SINGLE',0,'UPLOADING',0,UTC_TIMESTAMP(6)+INTERVAL 1 DAY)",
		"UPDATE users SET used_storage=1024,reserved_storage=517 WHERE id=7",
	} {
		execute(t, statement)
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	originalDevice := domain.Actor{UserID: fixture.UserID, DeviceID: fixture.DeviceID, SessionID: 17}
	otherDevice := domain.Actor{UserID: fixture.UserID, DeviceID: 15, SessionID: 18}
	reset := func(t *testing.T) {
		t.Helper()
		execute(t, `UPDATE upload_tasks SET status='UPLOADING',upload_mode='SINGLE',storage_upload_id=NULL,
			part_size=NULL,part_count=NULL,published_object_id=NULL,resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,
			reserved_bytes=5,expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 DAY,lease_owner='old-receiver',
			lease_until=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND,lease_version=?,next_attempt_at=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR,
			attempt_count=4,last_error_code='STORAGE_UNAVAILABLE',last_error_message='old diagnostic' WHERE id=?`,
			uint64(math.MaxUint64), fixture.ID)
	}
	type uploadFacts struct {
		task   *domain.UploadTask
		object *domain.StoredObject
		quota  [3]uint64
	}
	read := func(t *testing.T) uploadFacts {
		t.Helper()
		task, err := store.FindUserUploadTask(ctx, fixture.UserID, fixture.ID)
		if err != nil {
			t.Fatal(err)
		}
		object, err := store.FindUserStoredObject(ctx, fixture.UserID, fixture.StagedObjectID)
		if err != nil {
			t.Fatal(err)
		}
		current := uploadFacts{task: task, object: object}
		if err := pool.QueryRowContext(ctx, "SELECT storage_quota,used_storage,reserved_storage FROM users WHERE id=?", fixture.UserID).
			Scan(&current.quota[0], &current.quota[1], &current.quota[2]); err != nil {
			t.Fatal(err)
		}
		return current
	}

	// 2. 从事务外对照完整记录，核对跨设备受理、重复调用和终态复用。
	t.Run("other device starts cancellation and replay returns snapshot", func(t *testing.T) {
		reset(t)
		before := read(t)
		accepted, err := store.BeginUserUploadCancellation(ctx, otherDevice, fixture.ID)
		if err != nil || accepted == nil {
			t.Fatal("同账号另一设备未能受理取消", err)
		}
		expected := *before.task
		expected.Status, expected.UpdatedAt = domain.UploadStatusCancelling, accepted.UpdatedAt
		expected.LeaseOwner, expected.LeaseUntil, expected.NextAttemptAt = nil, nil, nil
		before.task = &expected
		replayed, err := store.BeginUserUploadCancellation(ctx, originalDevice, fixture.ID)
		if err != nil || !reflect.DeepEqual(accepted, &expected) || !reflect.DeepEqual(replayed, &expected) ||
			!reflect.DeepEqual(read(t), before) {
			t.Fatal("取消受理或重复请求改变了其他任务事实、对象或配额", err)
		}
	})
	t.Run("completed upload keeps publication result", func(t *testing.T) {
		reset(t)
		execute(t, `UPDATE upload_tasks SET status='COMPLETED',reserved_bytes=0,lease_owner=NULL,lease_until=NULL,
			next_attempt_at=NULL,expires_at='2000-01-01 00:00:00',completed_at=UTC_TIMESTAMP(6),
			published_object_id=29,result_entry_id=31,resolved_name='published.md' WHERE id=19`)
		before := read(t)
		actual, err := store.BeginUserUploadCancellation(ctx, originalDevice, fixture.ID)
		if err != nil || !reflect.DeepEqual(actual, before.task) || !reflect.DeepEqual(read(t), before) {
			t.Fatal("取消请求改变了已有发布结果", actual, err)
		}
	})
	t.Run("database clock lease and expiration equality", func(t *testing.T) {
		// 固定独占库的一条连接，使判断和写入使用相同的数据库时刻。
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		databaseTime := time.Now().UTC().Truncate(time.Second)
		execute(t, "SET timestamp = ?", databaseTime.Unix())
		defer execute(t, "SET timestamp = DEFAULT")
		reset(t)
		execute(t, "UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6) WHERE id=19")
		accepted, err := store.BeginUserUploadCancellation(ctx, originalDevice, fixture.ID)
		if err != nil || accepted == nil || accepted.Status != domain.UploadStatusCancelling || !accepted.UpdatedAt.Equal(databaseTime) {
			t.Fatal("数据库时间到达租约期限时未受理取消", accepted, err)
		}
		reset(t)
		execute(t, "UPDATE upload_tasks SET expires_at=UTC_TIMESTAMP(6) WHERE id=19")
		before := read(t)
		actual, err := store.BeginUserUploadCancellation(ctx, originalDevice, fixture.ID)
		if actual != nil || !errors.Is(err, domain.ErrUploadTaskNotReady) || !reflect.DeepEqual(read(t), before) {
			t.Fatal("数据库时间到达任务期限时仍尝试开始取消", actual, err)
		}
	})

	// 3. 验证冲突、归属和认证失败，以及读回异常触发回滚。
	for _, scenario := range []struct {
		name, patch string
		wantError   error
	}{
		{"live lease", "UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR WHERE id=19", domain.ErrUploadTaskBusy},
		{"expiration already accepted", "UPDATE upload_tasks SET status='EXPIRING' WHERE id=19", domain.ErrUploadTaskNotReady},
		{"multipart", "UPDATE upload_tasks SET upload_mode='MULTIPART',part_size=5242880,part_count=4 WHERE id=19", domain.ErrUploadTaskNotReady},
		{"readback rejects altered reservation", "CREATE TRIGGER fixture_cancel_restore BEFORE UPDATE ON upload_tasks FOR EACH ROW SET NEW.reserved_bytes=0", domain.ErrInvalidUploadTaskData},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			reset(t)
			execute(t, scenario.patch)
			before := read(t)
			actual, err := store.BeginUserUploadCancellation(ctx, originalDevice, fixture.ID)
			execute(t, "DROP TRIGGER IF EXISTS fixture_cancel_restore")
			if actual != nil || !errors.Is(err, scenario.wantError) || !reflect.DeepEqual(read(t), before) {
				t.Fatal("冲突返回或事务回滚结果不符", actual, err)
			}
		})
	}
	t.Run("invisible task and revoked session", func(t *testing.T) {
		reset(t)
		before := read(t)
		for _, taskID := range []uint64{20, 999} {
			if actual, err := store.BeginUserUploadCancellation(ctx, originalDevice, taskID); actual != nil || !errors.Is(err, domain.ErrUploadTaskNotFound) {
				t.Fatal("缺失和跨用户任务未统一处理", actual, err)
			}
		}
		execute(t, "UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=17")
		defer execute(t, "UPDATE auth_sessions SET revoked_at=NULL WHERE id=17")
		if actual, err := store.BeginUserUploadCancellation(ctx, originalDevice, fixture.ID); actual != nil || !errors.Is(err, domain.ErrSessionRevoked) {
			t.Fatal("已撤销会话仍可受理取消", actual, err)
		}
		if !reflect.DeepEqual(read(t), before) {
			t.Fatal("被拒绝的取消改变了任务或配额")
		}
	})

	// 4. 两个并发请求都读取同一受理结果，触发器核对状态只写一次。
	t.Run("concurrent requests write the status once", func(t *testing.T) {
		reset(t)
		execute(t, "CREATE TABLE fixture_cancel_writes(task_id BIGINT UNSIGNED NOT NULL)")
		execute(t, "CREATE TRIGGER fixture_count_cancels AFTER UPDATE ON upload_tasks FOR EACH ROW INSERT INTO fixture_cancel_writes(task_id) VALUES(NEW.id)")
		type outcome struct {
			task *domain.UploadTask
			err  error
		}
		startRequests := make(chan struct{})
		results := make(chan outcome, 2)
		for _, actor := range []domain.Actor{originalDevice, otherDevice} {
			go func() {
				<-startRequests
				task, err := store.BeginUserUploadCancellation(ctx, actor, fixture.ID)
				results <- outcome{task: task, err: err}
			}()
		}
		close(startRequests)
		first, second := <-results, <-results
		if first.err != nil || second.err != nil || first.task == nil || first.task.Status != domain.UploadStatusCancelling ||
			!reflect.DeepEqual(first.task, second.task) {
			t.Fatal("并发请求未复用同一已提交状态", first, second)
		}
		var writes int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM fixture_cancel_writes WHERE task_id=?", fixture.ID).Scan(&writes); err != nil || writes != 1 {
			t.Fatal("并发取消没有恰好写入一次", writes, err)
		}
	})
}
