package repository

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
)

func TestFindMultipartUploadCancellationTargetIntegration(t *testing.T) {
	ctx, pool, store := prepareMultipartCompletionDatabase(t)
	const provider, bucket, key = "local_minio", "fixture-bucket", "fixture/abort +%.bin"
	execute := func(t *testing.T, statement string, arguments ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
			t.Fatal(err)
		}
	}
	reset := func(t *testing.T) {
		t.Helper()
		resetMultipartUploadCancellationFixture(t, ctx, pool)
		execute(t, "UPDATE stored_objects SET provider=?,bucket=?,object_key=? WHERE id=23", provider, bucket, key)
	}
	claim := func(t *testing.T) *domain.UploadTask {
		t.Helper()
		candidates, err := store.ListMultipartUploadCancellationCandidates(ctx, 1)
		if err != nil || len(candidates) != 1 || candidates[0].TaskID != 19 {
			t.Fatal("未取得原任务的取消候选", candidates, err)
		}
		claimed, err := store.ClaimMultipartUploadCancellationLease(ctx, candidates[0], "fixture-cancel-worker")
		if err != nil {
			t.Fatal("未领取原任务的取消租约", err)
		}
		return claimed
	}
	type facts struct {
		task           *domain.UploadTask
		object         *domain.StoredObject
		used, reserved uint64
	}
	read := func(t *testing.T) facts {
		t.Helper()
		task, err := store.FindUserUploadTask(ctx, 7, 19)
		if err != nil {
			t.Fatal(err)
		}
		object, err := store.FindUserStoredObject(ctx, 7, 23)
		if err != nil {
			t.Fatal(err)
		}
		current := facts{task: task, object: object}
		if err := pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage FROM users WHERE id=7").Scan(&current.used, &current.reserved); err != nil {
			t.Fatal(err)
		}
		return current
	}

	// 1. 用正式租约读取原键及原会话，复核返回值与数据库记录相互独立。
	t.Run("original key and session", func(t *testing.T) {
		reset(t)
		claimed := claim(t)
		before := read(t)
		target, err := store.FindMultipartUploadCancellationTarget(ctx, claimed, provider, bucket)
		if err != nil || target == nil || target.ObjectKey != key || target.UploadID == nil || *target.UploadID != multipartCancellationFixtureSession {
			t.Fatal("未收到原样保存的对象键和会话", err)
		}
		*target.UploadID = "fixture modified result"
		if *claimed.StorageUploadID != multipartCancellationFixtureSession || !reflect.DeepEqual(before, read(t)) {
			t.Fatal("只读目标查询改变了租约、对象或配额")
		}
	})

	t.Run("expired upload with no session and disabled account", func(t *testing.T) {
		reset(t)
		execute(t, "UPDATE upload_tasks SET storage_upload_id=NULL WHERE id=19")
		execute(t, "UPDATE users SET status='DISABLED' WHERE id=7")
		claimed := claim(t)
		before := read(t)
		target, err := store.FindMultipartUploadCancellationTarget(ctx, claimed, provider, bucket)
		if err != nil || target == nil || target.ObjectKey != key || target.UploadID != nil || !reflect.DeepEqual(before, read(t)) {
			t.Fatal("已受理的原到期任务未按原未绑定目标返回", err)
		}
	})

	// 2. 固定独占连接的 MySQL 时间，验证可读取 VERIFYING 对象及租约到期边界。
	t.Run("lease time boundary with verifying object", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		now := time.Now().UTC().Truncate(time.Second)
		execute(t, "SET timestamp = ?", now.Unix())
		defer execute(t, "SET timestamp = DEFAULT")
		reset(t)
		execute(t, "UPDATE stored_objects SET status='VERIFYING',storage_etag='fixture-etag',storage_version_id='fixture-version' WHERE id=23")
		claimed := claim(t)
		before := read(t)
		execute(t, "SET timestamp = ?", claimed.LeaseUntil.Add(-time.Second).Unix())
		target, err := store.FindMultipartUploadCancellationTarget(ctx, claimed, provider, bucket)
		if err != nil || target == nil || target.ObjectKey != key || target.UploadID == nil || *target.UploadID != multipartCancellationFixtureSession {
			t.Fatal("有效取消租约应能读取待校验对象", err)
		}
		execute(t, "SET timestamp = ?", claimed.LeaseUntil.Unix())
		if target, err := store.FindMultipartUploadCancellationTarget(ctx, claimed, provider, bucket); target != nil ||
			!errors.Is(err, domain.ErrUploadTaskBusy) || !reflect.DeepEqual(before, read(t)) {
			t.Fatal("租约到期瞬间仍返回了目标或改动了数据库", err)
		}
		t.Log("租约到期前一秒可读取，恰好到期返回 UPLOAD_BUSY")
	})

	// 3. 写入仅由隔离夹具执行，生产查询拒绝已失配的快照且保持原数据。
	for _, scenario := range []struct {
		name, change string
		want         error
	}{
		{"lease owner changed", "UPDATE upload_tasks SET lease_owner='other-worker' WHERE id=19", domain.ErrUploadTaskBusy},
		{"lease deadline changed", "UPDATE upload_tasks SET lease_until=lease_until+INTERVAL 1 SECOND WHERE id=19", domain.ErrUploadTaskBusy},
		{"raw session changed", "UPDATE upload_tasks SET storage_upload_id=RTRIM(storage_upload_id) WHERE id=19", domain.ErrUploadTaskBusy},
		{"session changed to null", "UPDATE upload_tasks SET storage_upload_id=NULL WHERE id=19", domain.ErrUploadTaskBusy},
		{"retry postponed", "UPDATE upload_tasks SET next_attempt_at=UTC_TIMESTAMP(6)+INTERVAL 1 MINUTE WHERE id=19", domain.ErrUploadTaskBusy},
		{"object binding changed", "UPDATE upload_tasks SET staged_object_id=29 WHERE id=19", domain.ErrUploadTaskBusy},
		{"task no longer cancelling", "UPDATE upload_tasks SET status='EXPIRING' WHERE id=19", domain.ErrUploadTaskNotReady},
		{"reservation incomplete", "UPDATE upload_tasks SET reserved_bytes=reserved_bytes-1 WHERE id=19", domain.ErrInvalidUploadTaskData},
		{"object size changed", "UPDATE stored_objects SET file_size=file_size+1 WHERE id=23", domain.ErrInvalidStoredObjectData},
		{"object no longer staging or verifying", "UPDATE stored_objects SET status='READY',verified_sha256=UNHEX(REPEAT('ac',32)),verified_at=UTC_TIMESTAMP(6) WHERE id=23", domain.ErrInvalidStoredObjectData},
		{"bucket changed", "UPDATE stored_objects SET bucket='unrelated-bucket' WHERE id=23", domain.ErrInvalidStoredObjectData},
		{"provider changed", "UPDATE stored_objects SET provider='unrelated-provider' WHERE id=23", domain.ErrInvalidStoredObjectData},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			reset(t)
			claimed := claim(t)
			execute(t, scenario.change)
			before := read(t)
			if target, err := store.FindMultipartUploadCancellationTarget(ctx, claimed, provider, bucket); target != nil ||
				!errors.Is(err, scenario.want) || !reflect.DeepEqual(before, read(t)) {
				t.Fatal("拒绝目标的结果或只读边界不符", scenario.want, err)
			}
		})
	}

	t.Run("different account and canceled call", func(t *testing.T) {
		reset(t)
		claimed := claim(t)
		before := read(t)
		other := *claimed
		other.UserID = 8
		if target, err := store.FindMultipartUploadCancellationTarget(ctx, &other, provider, bucket); target != nil || !errors.Is(err, domain.ErrUploadTaskNotFound) {
			t.Fatal("其他账号取得了原对象定位", err)
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if target, err := store.FindMultipartUploadCancellationTarget(canceled, claimed, provider, bucket); target != nil ||
			!errors.Is(err, context.Canceled) || !reflect.DeepEqual(before, read(t)) {
			t.Fatal("取消未正常返回或改变了原数据", err)
		}
	})

	t.Run("lease replaced after object read", func(t *testing.T) {
		reset(t)
		claimed := claim(t)
		before := read(t)
		changed := false
		const callback = "fixture:replace_cancellation_lease_after_object_read"
		queries := store.database.Callback().Query()
		if err := queries.After("gorm:query").Register(callback, func(query *gorm.DB) {
			if query.Error == nil && query.Statement.Table == "stored_objects" && !changed {
				changed = true
				execute(t, "UPDATE upload_tasks SET lease_version=lease_version+1 WHERE id=19")
			}
		}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := queries.Remove(callback); err != nil {
				t.Error(err)
			}
		})
		target, err := store.FindMultipartUploadCancellationTarget(ctx, claimed, provider, bucket)
		before.task.LeaseVersion++
		if !changed || target != nil || !errors.Is(err, domain.ErrUploadTaskBusy) || !reflect.DeepEqual(before, read(t)) {
			t.Fatal("读取对象后未发现丢失租约，或产生了额外写入", err)
		}
		t.Log("读取暂存对象后替换租约版本，再次读任务时拒绝旧执行者")
	})
}
