package repository

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestBeginUserMultipartUploadCancellationIntegration(t *testing.T) {
	ctx, pool, store := prepareMultipartCompletionDatabase(t)
	const userID, taskID, objectID = 7, 19, 23
	const fileSize = domain.MaxSingleUploadSizeBytes + 1
	const rawSession = "Fixture+A/= "
	primary := domain.Actor{UserID: userID, DeviceID: 11, SessionID: 17}
	secondary := domain.Actor{UserID: userID, DeviceID: 15, SessionID: 18}
	outsider := domain.Actor{UserID: 8, DeviceID: 12, SessionID: 22}
	execute := func(t *testing.T, statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	reset := func(t *testing.T, status domain.UploadStatus) {
		t.Helper()
		// 1. 准备合法分片声明、当前对象和全额预占。
		session := new(rawSession)
		if status == domain.UploadStatusCreating {
			session = nil
		}
		objectStatus := domain.StoredObjectStatusStaging
		var etag, version *string
		if status == domain.UploadStatusVerifying {
			objectStatus = domain.StoredObjectStatusVerifying
			etag, version = new("fixture-etag"), new("fixture-version")
		}
		execute(t, `UPDATE stored_objects SET status=?,file_size=?,storage_etag=?,storage_version_id=? WHERE id=23`, objectStatus, fileSize, etag, version)
		execute(t, `UPDATE users SET used_storage=1024,reserved_storage=? WHERE id=7`, fileSize+512)
		execute(t, `UPDATE upload_tasks SET status=?,upload_mode='MULTIPART',storage_upload_id=?,
			file_size=?,part_size=?,part_count=2,expected_sha256=UNHEX(REPEAT('ab',32)),reserved_bytes=?,
			published_object_id=NULL,result_entry_id=NULL,resolved_name=NULL,completed_at=NULL,
			lease_owner='old-worker',lease_version=4,lease_until=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND,
			next_attempt_at=UTC_TIMESTAMP(6)+INTERVAL 1 MINUTE,attempt_count=2,
			last_error_code='STORAGE_UNAVAILABLE',last_error_message='fixture diagnostic',
			expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR,updated_at=UTC_TIMESTAMP(6) WHERE id=19`, status, session, fileSize, fileSize-1, fileSize)
	}
	type facts struct {
		task    *domain.UploadTask
		object  *domain.StoredObject
		account [2]uint64
	}
	read := func(t *testing.T) facts {
		t.Helper()
		task, err := store.FindUserUploadTask(ctx, userID, taskID)
		if err != nil {
			t.Fatal(err)
		}
		object, err := store.FindUserStoredObject(ctx, userID, objectID)
		if err != nil {
			t.Fatal(err)
		}
		result := facts{task: task, object: object}
		if err := pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage FROM users WHERE id=?", userID).Scan(&result.account[0], &result.account[1]); err != nil {
			t.Fatal(err)
		}
		return result
	}
	assertAccepted := func(t *testing.T, before facts, accepted *domain.UploadTask, err error) {
		t.Helper()
		if err != nil || accepted == nil || accepted.UpdatedAt.Before(before.task.UpdatedAt) {
			t.Fatal("未受理分片取消", accepted, err)
		}
		expected := *before.task
		expected.Status, expected.UpdatedAt = domain.UploadStatusCancelling, accepted.UpdatedAt
		expected.LeaseOwner, expected.LeaseUntil, expected.NextAttemptAt = nil, nil, nil
		before.task = &expected
		if !reflect.DeepEqual(accepted, &expected) || !reflect.DeepEqual(read(t), before) {
			t.Fatal("受理改变了原会话、分片声明、对象或已预占额度", accepted)
		}
	}

	// 2. 各在途阶段可受理，同账号另一设备及重复请求复用提交结果。
	for _, status := range []domain.UploadStatus{
		domain.UploadStatusCreating, domain.UploadStatusUploading, domain.UploadStatusCompleting, domain.UploadStatusVerifying,
	} {
		t.Run(string(status), func(t *testing.T) {
			reset(t, status)
			before := read(t)
			accepted, err := store.BeginUserMultipartUploadCancellation(ctx, secondary, taskID)
			assertAccepted(t, before, accepted, err)
			after := read(t)
			repeated, err := store.BeginUserMultipartUploadCancellation(ctx, primary, taskID)
			if err != nil || !reflect.DeepEqual(repeated, accepted) || !reflect.DeepEqual(read(t), after) {
				t.Fatal("重复受理未复用当前任务", repeated, err)
			}
		})
	}
	t.Run("publication committed first", func(t *testing.T) {
		reset(t, domain.UploadStatusVerifying)
		execute(t, `UPDATE upload_tasks SET status='COMPLETED',reserved_bytes=0,lease_owner=NULL,lease_until=NULL,
			next_attempt_at=NULL,expires_at=UTC_TIMESTAMP(6),completed_at=UTC_TIMESTAMP(6),
			published_object_id=29,result_entry_id=31,resolved_name='published.md' WHERE id=19`)
		execute(t, "UPDATE users SET used_storage=?,reserved_storage=512 WHERE id=7", fileSize+1024)
		before := read(t)
		actual, err := store.BeginUserMultipartUploadCancellation(ctx, primary, taskID)
		if err != nil || !reflect.DeepEqual(actual, before.task) || !reflect.DeepEqual(read(t), before) {
			t.Fatal("取消覆盖了已提交的发布结果", actual, err)
		}
	})
	t.Run("lease and task expiration boundaries", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		databaseTime := time.Now().UTC().Truncate(time.Second)
		execute(t, "SET timestamp = ?", databaseTime.Unix())
		defer execute(t, "SET timestamp = DEFAULT")
		reset(t, domain.UploadStatusVerifying)
		execute(t, "UPDATE upload_tasks SET lease_owner=NULL,lease_until=NULL,next_attempt_at=NULL WHERE id=19")
		candidates, err := store.ListMultipartUploadVerificationCandidates(ctx, 1)
		if err != nil || len(candidates) != 1 {
			t.Fatal("未读到测试校验候选", candidates, err)
		}
		claimed, err := store.ClaimMultipartUploadVerificationLease(ctx, candidates[0], "fixture-verification")
		if err != nil || claimed == nil || claimed.LeaseUntil == nil {
			t.Fatal("未领取用于竞争测试的真实租约", claimed, err)
		}
		execute(t, "SET timestamp = ?", claimed.LeaseUntil.Unix())
		before := read(t)
		accepted, err := store.BeginUserMultipartUploadCancellation(ctx, primary, taskID)
		assertAccepted(t, before, accepted, err)
		if !accepted.UpdatedAt.Equal(*claimed.LeaseUntil) {
			t.Fatal("未在租约恰好到期时受理", accepted.UpdatedAt)
		}
		digest := domain.UploadContentDigest{SizeBytes: fileSize, SHA256: *claimed.ExpectedSHA256}
		err = store.WithUploadVerificationTransaction(ctx, claimed, before.object, digest,
			func(context.Context, *Store, *domain.UploadTask, *domain.StoredObject) error {
				t.Error("取消提交后旧校验仍进入发布回调")
				return nil
			})
		if !errors.Is(err, domain.ErrUploadTaskNotReady) {
			t.Fatal("取消提交后未拒绝旧校验", err)
		}
		reset(t, domain.UploadStatusCompleting)
		execute(t, "UPDATE upload_tasks SET expires_at=UTC_TIMESTAMP(6) WHERE id=19")
		before = read(t)
		if actual, err := store.BeginUserMultipartUploadCancellation(ctx, primary, taskID); actual != nil ||
			!errors.Is(err, domain.ErrUploadTaskNotReady) || !reflect.DeepEqual(read(t), before) {
			t.Fatal("任务恰好到期时仍受理了取消", actual, err)
		}
	})

	// 3. 检查冲突、归属、读回回滚及并发下恰好一次的状态写入。
	for _, scenario := range []struct {
		name, change string
		want         error
	}{
		{"active lease", "UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6)+INTERVAL 1 MINUTE WHERE id=19", domain.ErrUploadTaskBusy},
		{"expiration already accepted", "UPDATE upload_tasks SET status='EXPIRING' WHERE id=19", domain.ErrUploadTaskNotReady},
		{"incomplete reservation", "UPDATE upload_tasks SET reserved_bytes=reserved_bytes-1 WHERE id=19", domain.ErrInvalidUploadTaskData},
		{"readback detects changed raw session", "CREATE TRIGGER fixture_cancel_session BEFORE UPDATE ON upload_tasks FOR EACH ROW SET NEW.storage_upload_id=RTRIM(NEW.storage_upload_id)", domain.ErrInvalidUploadTaskData},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			reset(t, domain.UploadStatusUploading)
			execute(t, scenario.change)
			defer execute(t, "DROP TRIGGER IF EXISTS fixture_cancel_session")
			before := read(t)
			actual, err := store.BeginUserMultipartUploadCancellation(ctx, primary, taskID)
			if actual != nil || !errors.Is(err, scenario.want) || !reflect.DeepEqual(read(t), before) {
				t.Fatal("拒绝或回滚未保留原任务、对象与配额", actual, err)
			}
		})
	}
	t.Run("other account and revoked session", func(t *testing.T) {
		reset(t, domain.UploadStatusUploading)
		before := read(t)
		if actual, err := store.BeginUserMultipartUploadCancellation(ctx, outsider, taskID); actual != nil || !errors.Is(err, domain.ErrUploadTaskNotFound) {
			t.Fatal("跨账号请求取得分片任务", actual, err)
		}
		execute(t, "UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=17")
		defer execute(t, "UPDATE auth_sessions SET revoked_at=NULL WHERE id=17")
		if actual, err := store.BeginUserMultipartUploadCancellation(ctx, primary, taskID); actual != nil || !errors.Is(err, domain.ErrSessionRevoked) ||
			!reflect.DeepEqual(read(t), before) {
			t.Fatal("失效会话改变了分片任务", actual, err)
		}
	})
	t.Run("concurrent requests write once", func(t *testing.T) {
		reset(t, domain.UploadStatusCompleting)
		before := read(t)
		execute(t, "CREATE TABLE fixture_cancel_writes(task_id BIGINT UNSIGNED NOT NULL)")
		execute(t, "CREATE TRIGGER fixture_count_cancels AFTER UPDATE ON upload_tasks FOR EACH ROW INSERT INTO fixture_cancel_writes(task_id) VALUES(NEW.id)")
		defer execute(t, "DROP TRIGGER fixture_count_cancels")
		type outcome struct {
			task *domain.UploadTask
			err  error
		}
		start, results := make(chan struct{}), make(chan outcome, 2)
		for _, actor := range []domain.Actor{primary, secondary} {
			go func() {
				<-start
				task, err := store.BeginUserMultipartUploadCancellation(ctx, actor, taskID)
				results <- outcome{task, err}
			}()
		}
		close(start)
		first, second := <-results, <-results
		assertAccepted(t, before, first.task, first.err)
		var writes int
		if second.err != nil || !reflect.DeepEqual(second.task, first.task) ||
			pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM fixture_cancel_writes WHERE task_id=?", taskID).Scan(&writes) != nil || writes != 1 {
			t.Fatal("并发取消未复用同一结果或重复写入", first, second, writes)
		}
	})
}
