package repository

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestReadMultipartUploadSessionProtectionIntegration(t *testing.T) {
	ctx, pool, store := prepareMultipartCompletionDatabase(t)
	pool.SetMaxOpenConns(1)
	const provider, bucket = "local_minio", "fixture-bucket"
	const key = "users/7/objects/4b351d22-0cab-4a8d-89cc-9fe9d0641e25"
	session := domain.MultipartUploadSession{ObjectKey: key, UploadID: "Fixture+A/= "}
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC().Truncate(time.Second)
	execute("SET timestamp = ?", now.Unix())
	defer execute("SET timestamp = DEFAULT")
	resetMultipartUploadCancellationFixture(t, ctx, pool)
	execute("UPDATE stored_objects SET provider=?,bucket=?,object_key=? WHERE id=23", provider, bucket, key)
	execute("UPDATE upload_tasks SET status='CREATING',storage_upload_id=NULL,expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 DAY WHERE id=19")
	page, err := store.ListMultipartUploadSessionScanTargets(ctx, provider, bucket, 0, 1)
	if err != nil || page == nil || len(page.Targets) != 1 || page.Targets[0].TaskID != 19 {
		t.Fatal("未取得原会话扫描目标", page, err)
	}
	target := page.Targets[0]
	check := func(t *testing.T, want domain.MultipartUploadSessionProtection) {
		t.Helper()
		got, err := store.ReadMultipartUploadSessionProtection(ctx, provider, bucket, target, session)
		if err != nil || got != want {
			t.Fatal("绑定或创建租约保护不符", got, err)
		}
	}

	// 1. 使用正式领取入口确认，存储尚未绑定时也有创建保护。
	check(t, domain.MultipartUploadSessionProtection{})
	candidates, err := store.ListMultipartUploadInitializationCandidates(ctx, 1)
	if err != nil || len(candidates) != 1 || candidates[0].TaskID != target.TaskID {
		t.Fatal("未取得合成初始化候选", candidates, err)
	}
	claimed, err := store.ClaimMultipartUploadInitializationLease(ctx, candidates[0], "fixture-creation-worker")
	if err != nil {
		t.Fatal(err)
	}
	check(t, domain.MultipartUploadSessionProtection{ActiveCreationLease: true})
	if bound, err := store.IsMultipartUploadSessionBound(ctx, provider, bucket, target, session); bound || err != nil {
		t.Fatal("已有绑定入口应继续只报告绑定事实", bound, err)
	}

	// 2. 保持数据库时钟固定，用微秒刻度验证两个截止点。
	for _, fixture := range []struct {
		name            string
		status          domain.UploadStatus
		lease, deadline time.Time
		active          bool
	}{
		{"lease still valid by one microsecond", domain.UploadStatusCreating, now.Add(time.Microsecond), claimed.ExpiresAt, true},
		{"lease ends now", domain.UploadStatusCreating, now, claimed.ExpiresAt, false},
		{"lease ended", domain.UploadStatusCreating, now.Add(-time.Microsecond), claimed.ExpiresAt, false},
		{"upload still valid by one microsecond", domain.UploadStatusCreating, *claimed.LeaseUntil, now.Add(time.Microsecond), true},
		{"upload expires now", domain.UploadStatusCreating, *claimed.LeaseUntil, now, false},
		{"upload already expired", domain.UploadStatusCreating, *claimed.LeaseUntil, now.Add(-time.Microsecond), false},
		{"expiration lease does not act as a creation lease", domain.UploadStatusExpiring, *claimed.LeaseUntil, now, false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			execute("UPDATE upload_tasks SET status=?,lease_until=?,expires_at=? WHERE id=19", fixture.status, fixture.lease, fixture.deadline)
			check(t, domain.MultipartUploadSessionProtection{ActiveCreationLease: fixture.active})
		})
	}
	execute("UPDATE upload_tasks SET status='CREATING',lease_until=?,expires_at=?,lease_owner='' WHERE id=19", *claimed.LeaseUntil, claimed.ExpiresAt)
	if got, err := store.ReadMultipartUploadSessionProtection(ctx, provider, bucket, target, session); got != (domain.MultipartUploadSessionProtection{}) || !errors.Is(err, domain.ErrInvalidUploadTaskData) {
		t.Fatal("异常租约被当作无保护结果", got, err)
	}
	execute("UPDATE upload_tasks SET lease_owner=? WHERE id=19", *claimed.LeaseOwner)

	// 3. 绑定提交后只报告原会话绑定，复核前后记录一致。
	bound, err := store.BindMultipartUploadSession(ctx, claimed, session.UploadID)
	if err != nil {
		t.Fatal(err)
	}
	object, err := store.FindUserStoredObject(ctx, target.UserID, target.ObjectID)
	if err != nil {
		t.Fatal(err)
	}
	check(t, domain.MultipartUploadSessionProtection{Bound: true})
	afterTask, err := store.FindUserUploadTask(ctx, target.UserID, target.TaskID)
	if err != nil || !reflect.DeepEqual(bound, afterTask) {
		t.Fatal("只读复核改变了任务", err)
	}
	afterObject, err := store.FindUserStoredObject(ctx, target.UserID, target.ObjectID)
	if err != nil || !reflect.DeepEqual(object, afterObject) {
		t.Fatal("只读复核改变了原对象", err)
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if _, err := store.ReadMultipartUploadSessionProtection(canceled, provider, bucket, target, session); !errors.Is(err, context.Canceled) {
		t.Fatal("取消原因未保留", err)
	}
	t.Log("数据库时间距任一截止一微秒时保护仍有效，恰好到期时失效，正式绑定提交后仅保留绑定保护")
}
