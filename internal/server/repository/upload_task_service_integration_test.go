package repository

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestUploadTaskServiceIntegration(t *testing.T) {
	// 1. 复用正式任务及关联夹具，将真实 Repository 方法绑定到服务。
	pool, want := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	tasks := service.NewUploadTaskService(store.FindUserUploadTask)
	actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
	check := func(t *testing.T, identity domain.Actor) *domain.UploadTask {
		t.Helper()
		actual, err := tasks.Get(ctx, identity, want.ID)
		if err != nil || !reflect.DeepEqual(actual, &want) {
			t.Fatal("真实任务服务交付错误", err)
		}
		return actual
	}
	check(t, actor)

	// 2. 用户归属保持一致时，其他设备也可读取原设备创建的任务。
	t.Run("another device of same user", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(15,7,'58ee758a-0727-42de-ae7d-3bb33155e148','another device','ANDROID')"); err != nil {
			t.Fatal(err)
		}
		otherActor := domain.Actor{UserID: 7, DeviceID: 15, SessionID: 18}
		if actual := check(t, otherActor); actual.DeviceID != 11 {
			t.Fatal("读取服务改变了任务来源设备")
		}
	})
	for _, status := range []domain.UploadStatus{
		domain.UploadStatusCreating, domain.UploadStatusUploading, domain.UploadStatusCompleting,
		domain.UploadStatusVerifying, domain.UploadStatusCompleted, domain.UploadStatusCancelling,
		domain.UploadStatusCancelled, domain.UploadStatusExpiring, domain.UploadStatusExpired, domain.UploadStatusFailed,
	} {
		t.Run(string(status), func(t *testing.T) {
			if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET status=? WHERE user_id=7 AND id=19", string(status)); err != nil {
				t.Fatal(err)
			}
			want.Status = status
			check(t, actor)
		})
	}
	t.Run("expired task and independent copies", func(t *testing.T) {
		want.ExpiresAt = want.CreatedAt.Add(-24 * time.Hour)
		if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET expires_at=? WHERE user_id=7 AND id=19", want.ExpiresAt); err != nil {
			t.Fatal(err)
		}
		actual := check(t, actor)
		actual.ExpectedSHA256[0] ^= 0xff
		*actual.ResolvedName, *actual.LeaseOwner, *actual.PublishedObjectID = "changed", "changed", 999
		check(t, actor)
	})

	// 3. 缺失或跨用户任务保留缺失原因，预先取消保留上下文错误。
	t.Run("missing and cross-user tasks", func(t *testing.T) {
		for _, query := range []struct {
			identity domain.Actor
			taskID   uint64
		}{
			{actor, 999},
			{domain.Actor{UserID: 8, DeviceID: 12, SessionID: 18}, 19},
		} {
			actual, err := tasks.Get(ctx, query.identity, query.taskID)
			if actual != nil || !errors.Is(err, domain.ErrUploadTaskNotFound) {
				t.Fatal("任务服务的缺失或跨账号结果错误", err)
			}
		}
	})
	t.Run("cancelled request", func(t *testing.T) {
		cancelled, stop := context.WithCancel(ctx)
		stop()
		actual, err := tasks.Get(cancelled, actor, 19)
		if actual != nil || !errors.Is(err, context.Canceled) {
			t.Fatal("真实任务服务丢失取消原因", err)
		}
	})
	t.Run("closed pool failure", func(t *testing.T) {
		if err := pool.Close(); err != nil {
			t.Fatal(err)
		}
		actual, err := tasks.Get(ctx, actor, 19)
		if actual != nil || err == nil || errors.Is(err, domain.ErrUploadTaskNotFound) {
			t.Fatal("任务服务混淆数据库故障和任务缺失", err)
		}
	})
}

func TestUploadTaskServiceConfirmCompletionIntegration(t *testing.T) {
	// 1. 用独占 MySQL 的正式任务表和读取方法装配确认服务。
	pool, fixture := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	tasks := service.NewUploadTaskService(store.FindUserUploadTask)
	actor := domain.Actor{UserID: fixture.UserID, DeviceID: 15, SessionID: 17}
	if result, err := tasks.ConfirmCompletion(ctx, actor, fixture.ID); result != nil || !errors.Is(err, domain.ErrUploadTaskNotReady) {
		t.Fatal("分片任务未返回任务未就绪", result, err)
	}
	if _, err := pool.ExecContext(ctx, `UPDATE upload_tasks SET upload_mode='SINGLE', storage_upload_id=NULL,
		part_size=NULL, part_count=NULL, status='VERIFYING', published_object_id=NULL, resolved_name=NULL,
		result_entry_id=NULL, completed_at=NULL, reserved_bytes=file_size, lease_owner=NULL, lease_until=NULL
		WHERE user_id=? AND id=?`, fixture.UserID, fixture.ID); err != nil {
		t.Fatal(err)
	}

	// 2. 同账号其他设备可确认校验中任务，缺失任务和跨用户查询使用同一错误。
	verifying, err := tasks.ConfirmCompletion(ctx, actor, fixture.ID)
	if err != nil || verifying == nil || verifying.Status != domain.UploadStatusVerifying || verifying.DeviceID != fixture.DeviceID ||
		verifying.ReservedBytes != fixture.FileSize || !reflect.DeepEqual(verifying.NextAttemptAt, fixture.NextAttemptAt) {
		t.Fatal("真实完成确认未保留校验或重试快照", verifying, err)
	}
	otherActor := domain.Actor{UserID: 8, DeviceID: 12, SessionID: 18}
	if result, err := tasks.ConfirmCompletion(ctx, otherActor, fixture.ID); result != nil || !errors.Is(err, domain.ErrUploadTaskNotFound) {
		t.Fatal("跨用户确认未沿用任务缺失错误", result, err)
	}
	if result, err := tasks.ConfirmCompletion(ctx, actor, 999); result != nil || !errors.Is(err, domain.ErrUploadTaskNotFound) {
		t.Fatal("缺失任务确认未返回预期错误", result, err)
	}

	// 3. 模拟后台已提交发布，重复确认应交回原结果，保留先前交付的快照。
	if _, err := pool.ExecContext(ctx, `UPDATE upload_tasks SET status='COMPLETED', reserved_bytes=0,
		published_object_id=?, result_entry_id=?, resolved_name=?, completed_at=?, updated_at=?,
		next_attempt_at=NULL, last_error_code=NULL, last_error_message=NULL WHERE user_id=? AND id=?`,
		*fixture.PublishedObjectID, *fixture.ResultEntryID, *fixture.ResolvedName, *fixture.CompletedAt, *fixture.CompletedAt,
		fixture.UserID, fixture.ID); err != nil {
		t.Fatal(err)
	}
	completed, err := tasks.ConfirmCompletion(ctx, actor, fixture.ID)
	if err != nil || completed == nil || completed.Status != domain.UploadStatusCompleted || completed.ResultEntryID == nil ||
		*completed.ResultEntryID != *fixture.ResultEntryID || !reflect.DeepEqual(completed.CompletedAt, fixture.CompletedAt) ||
		completed.ReservedBytes != 0 || verifying.Status != domain.UploadStatusVerifying || verifying.ResultEntryID != nil {
		t.Fatal("真实发布结果或上一份校验快照不符", completed, verifying, err)
	}
	replayed, err := tasks.ConfirmCompletion(ctx, actor, fixture.ID)
	if err != nil || !reflect.DeepEqual(replayed, completed) || replayed == completed || replayed.ResultEntryID == completed.ResultEntryID {
		t.Fatal("重复确认未返回同一发布结果的独立副本", replayed, err)
	}
}
