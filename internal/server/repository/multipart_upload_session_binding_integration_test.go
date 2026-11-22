package repository

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestIsMultipartUploadSessionBoundIntegration(t *testing.T) {
	ctx, pool, store := prepareMultipartCompletionDatabase(t)
	const provider, bucket = "local_minio", "fixture-bucket"
	const objectKey = "users/7/objects/4b351d22-0cab-4a8d-89cc-9fe9d0641e25"
	const uploadID = "Fixture+A/= "
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	resetMultipartUploadCancellationFixture(t, ctx, pool)
	execute("UPDATE stored_objects SET provider=?,bucket=?,object_key=? WHERE id=23", provider, bucket, objectKey)
	execute("UPDATE upload_tasks SET status='CREATING',storage_upload_id=NULL,expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 DAY WHERE id=19")

	// 1. 使用正式扫描结果。会话在扫描之后通过真实初始化事务完成绑定。
	page, err := store.ListMultipartUploadSessionScanTargets(ctx, provider, bucket, 0, 1)
	if err != nil || page == nil || len(page.Targets) != 1 || page.Targets[0].TaskID != 19 {
		t.Fatal("未读取到合成目标", page, err)
	}
	target := page.Targets[0]
	observed := domain.MultipartUploadSession{ObjectKey: objectKey, UploadID: uploadID}
	if bound, err := store.IsMultipartUploadSessionBound(ctx, provider, bucket, target, observed); bound || err != nil {
		t.Fatal("空绑定被识别为已持有会话", bound, err)
	}
	candidates, err := store.ListMultipartUploadInitializationCandidates(ctx, 1)
	if err != nil || len(candidates) != 1 || candidates[0].TaskID != target.TaskID {
		t.Fatal("未取到合成初始化候选", candidates, err)
	}
	claimed, err := store.ClaimMultipartUploadInitializationLease(ctx, candidates[0], "fixture-binder")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindMultipartUploadSession(ctx, claimed, uploadID); err != nil {
		t.Fatal(err)
	}
	if bound, err := store.IsMultipartUploadSessionBound(ctx, provider, bucket, target, observed); !bound || err != nil {
		t.Fatal("复核未读到扫描后提交的绑定", bound, err)
	}

	// 2. 保留终态的已登记会话，按原值区分大小写和末尾空格。
	execute("UPDATE upload_tasks SET status='EXPIRED',reserved_bytes=0,completed_at=UTC_TIMESTAMP(6) WHERE id=19")
	readTask := func() *domain.UploadTask {
		t.Helper()
		task, err := store.FindUserUploadTask(ctx, target.UserID, target.TaskID)
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	readObject := func() *domain.StoredObject {
		t.Helper()
		object, err := store.FindUserStoredObject(ctx, target.UserID, target.ObjectID)
		if err != nil {
			t.Fatal(err)
		}
		return object
	}
	readReservation := func() uint64 {
		t.Helper()
		var reserved uint64
		if err := pool.QueryRowContext(ctx, "SELECT reserved_storage FROM users WHERE id=?", target.UserID).Scan(&reserved); err != nil {
			t.Fatal(err)
		}
		return reserved
	}
	beforeTask, beforeObject, beforeReserved := readTask(), readObject(), readReservation()
	for _, fixture := range []struct {
		id   string
		want bool
	}{
		{uploadID, true},
		{strings.TrimSuffix(uploadID, " "), false},
		{strings.ToLower(uploadID), false},
		{"another-session-for-this-key", false},
	} {
		candidate := observed
		candidate.UploadID = fixture.id
		bound, err := store.IsMultipartUploadSessionBound(ctx, provider, bucket, target, candidate)
		if err != nil || bound != fixture.want {
			t.Fatal("会话 ID 比较结果不符", bound, err)
		}
	}
	if !reflect.DeepEqual(beforeTask, readTask()) || !reflect.DeepEqual(beforeObject, readObject()) || beforeReserved != readReservation() {
		t.Fatal("只读复核修改了任务、对象或预占")
	}
	for _, fixture := range []struct {
		name, provider, bucket string
		edit                   func(*domain.MultipartUploadSessionScanTarget, *domain.MultipartUploadSession)
		want                   error
	}{
		{name: "changed task", edit: func(target *domain.MultipartUploadSessionScanTarget, _ *domain.MultipartUploadSession) {
			target.TaskID++
		}, want: domain.ErrUploadTaskNotFound},
		{name: "changed object", edit: func(target *domain.MultipartUploadSessionScanTarget, _ *domain.MultipartUploadSession) {
			target.ObjectID++
		}, want: domain.ErrUploadTaskNotFound},
		{name: "wrong user", edit: func(target *domain.MultipartUploadSessionScanTarget, session *domain.MultipartUploadSession) {
			target.UserID = 8
			target.ObjectKey = strings.Replace(target.ObjectKey, "users/7/", "users/8/", 1)
			session.ObjectKey = target.ObjectKey
		}, want: domain.ErrUploadTaskNotFound},
		{name: "owner mismatched to key", edit: func(target *domain.MultipartUploadSessionScanTarget, _ *domain.MultipartUploadSession) {
			target.UserID = 8
		}, want: domain.ErrInvalidStoredObjectData},
		{name: "wrong session key", edit: func(_ *domain.MultipartUploadSessionScanTarget, session *domain.MultipartUploadSession) {
			session.ObjectKey += "-other"
		}, want: domain.ErrInvalidStoredObjectData},
		{name: "wrong provider", provider: provider + " ", want: domain.ErrUploadTaskNotFound},
		{name: "wrong bucket", bucket: "another-bucket", want: domain.ErrUploadTaskNotFound},
		{name: "invalid upload ID", edit: func(_ *domain.MultipartUploadSessionScanTarget, session *domain.MultipartUploadSession) {
			session.UploadID = " \n"
		}, want: domain.ErrInvalidUploadTaskData},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			currentTarget, currentSession := target, observed
			if fixture.edit != nil {
				fixture.edit(&currentTarget, &currentSession)
			}
			currentProvider, currentBucket := provider, bucket
			if fixture.provider != "" {
				currentProvider = fixture.provider
			}
			if fixture.bucket != "" {
				currentBucket = fixture.bucket
			}
			bound, err := store.IsMultipartUploadSessionBound(ctx, currentProvider, currentBucket, currentTarget, currentSession)
			if bound || !errors.Is(err, fixture.want) {
				t.Fatal("无法确认的会话未被拒绝", bound, err)
			}
		})
	}

	// 3. 原记录变化或数据库里的绑定异常时，只返回错误。
	execute("UPDATE stored_objects SET object_key=CONCAT(object_key,' ') WHERE id=23")
	if bound, err := store.IsMultipartUploadSessionBound(ctx, provider, bucket, target, observed); bound || !errors.Is(err, domain.ErrUploadTaskNotFound) {
		t.Fatal("原键发生变化后仍接受旧扫描目标", bound, err)
	}
	execute("UPDATE stored_objects SET object_key=? WHERE id=23", objectKey)
	execute("UPDATE upload_tasks SET storage_upload_id=' ' WHERE id=19")
	if bound, err := store.IsMultipartUploadSessionBound(ctx, provider, bucket, target, observed); bound || !errors.Is(err, domain.ErrInvalidUploadTaskData) {
		t.Fatal("异常的已登记会话被当作未绑定", bound, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.IsMultipartUploadSessionBound(canceled, provider, bucket, target, observed); !errors.Is(err, context.Canceled) {
		t.Fatal("取消原因未保留", err)
	}
	if err := store.WithLoginUserTransaction(ctx, target.UserID, func(txContext context.Context, txStore *Store) error {
		if _, err := txStore.IsMultipartUploadSessionBound(txContext, provider, bucket, target, observed); err == nil {
			t.Error("只读复核接受了事务内的快照")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Log("真实初始化事务绑定后重新复核，终态原会话仍受保护，任务、对象和预占保持原值")
}
