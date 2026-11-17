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
