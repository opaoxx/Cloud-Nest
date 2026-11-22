package repository

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestRecheckMultipartUploadSessionCleanupIntegration(t *testing.T) {
	ctx, pool, store := prepareMultipartCompletionDatabase(t)
	const provider, bucket = "local_minio", "fixture-bucket"
	const key = "users/7/objects/4b351d22-0cab-4a8d-89cc-9fe9d0641e25"
	const grace = 15 * time.Minute
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	resetMultipartUploadCancellationFixture(t, ctx, pool)
	execute("UPDATE stored_objects SET provider=?,bucket=?,object_key=? WHERE id=23", provider, bucket, key)
	execute("UPDATE upload_tasks SET status='CREATING',storage_upload_id=NULL,expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 DAY WHERE id=19")
	page, err := store.ListMultipartUploadSessionScanTargets(ctx, provider, bucket, 0, 1)
	if err != nil || page == nil || len(page.Targets) != 1 {
		t.Fatal("未取得原对象目标", page, err)
	}
	target := page.Targets[0]
	session := domain.MultipartUploadSession{ObjectKey: key, UploadID: multipartCancellationFixtureSession, InitiatedAt: time.Now().UTC().Add(-time.Hour)}
	check := func(t *testing.T, candidate domain.MultipartUploadSession, wantStatus domain.UploadStatus, wantProtection domain.MultipartUploadSessionProtection, elapsed, eligible bool) {
		t.Helper()
		got, err := store.RecheckMultipartUploadSessionCleanup(ctx, provider, bucket, target, candidate, grace)
		if err != nil || got.TaskStatus != wantStatus || got.Facts.Protection != wantProtection || got.Facts.GraceElapsed != elapsed || got.Eligible() != eligible {
			t.Fatal("加锁后任务状态、保护或收尾条件不符", got, err)
		}
	}

	// 1. 当前没有租约也须推迟。用正式入口证明该任务随后仍能领取租约和绑定。
	check(t, session, domain.UploadStatusCreating, domain.MultipartUploadSessionProtection{}, true, false)
	candidates, err := store.ListMultipartUploadInitializationCandidates(ctx, 1)
	if err != nil || len(candidates) != 1 || candidates[0].TaskID != target.TaskID {
		t.Fatal("未取得可领取的初始化任务", candidates, err)
	}
	claimed, err := store.ClaimMultipartUploadInitializationLease(ctx, candidates[0], "fixture-locked-check")
	if err != nil {
		t.Fatal(err)
	}
	check(t, session, domain.UploadStatusCreating, domain.MultipartUploadSessionProtection{ActiveCreationLease: true}, true, false)
	execute("UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6) WHERE id=19")
	check(t, session, domain.UploadStatusCreating, domain.MultipartUploadSessionProtection{}, true, false)
	execute("UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6)+INTERVAL 1 MINUTE WHERE id=19")
	bound, err := store.BindMultipartUploadSession(ctx, claimed, session.UploadID)
	if err != nil {
		t.Fatal(err)
	}
	check(t, session, domain.UploadStatusUploading, domain.MultipartUploadSessionProtection{Bound: true}, true, false)
	other := session
	other.UploadID = strings.TrimSuffix(session.UploadID, " ")
	originalObject, err := store.FindUserStoredObject(ctx, target.UserID, target.ObjectID)
	if err != nil {
		t.Fatal(err)
	}
	readReservation := func() uint64 {
		t.Helper()
		var reserved uint64
		if err := pool.QueryRowContext(ctx, "SELECT reserved_storage FROM users WHERE id=?", target.UserID).Scan(&reserved); err != nil {
			t.Fatal(err)
		}
		return reserved
	}
	reserved := readReservation()
	check(t, other, domain.UploadStatusUploading, domain.MultipartUploadSessionProtection{}, true, true)
	recent := other
	recent.InitiatedAt = time.Now().UTC()
	check(t, recent, domain.UploadStatusUploading, domain.MultipartUploadSessionProtection{}, false, false)

	// 2. 分别持有三行的外部锁，验证复核等待、服从期限并在锁释放后可重试。
	for _, fixture := range []struct {
		name  string
		query string
		id    uint64
	}{
		{"account", "SELECT id FROM users WHERE id=? FOR UPDATE", target.UserID},
		{"task", "SELECT id FROM upload_tasks WHERE id=? FOR UPDATE", target.TaskID},
		{"object", "SELECT id FROM stored_objects WHERE id=? FOR UPDATE", target.ObjectID},
	} {
		t.Run(fixture.name+" lock", func(t *testing.T) {
			tx, err := pool.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			var lockedID uint64
			if err := tx.QueryRowContext(ctx, fixture.query, fixture.id).Scan(&lockedID); err != nil || lockedID != fixture.id {
				t.Fatal("夹具未能持有预期行锁", lockedID, err)
			}
			waiting, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
			defer cancel()
			got, err := store.RecheckMultipartUploadSessionCleanup(waiting, provider, bucket, target, other, grace)
			if !errors.Is(err, context.DeadlineExceeded) || got != (domain.MultipartUploadSessionLockedCheck{}) {
				t.Fatal("行锁未阻止复核提前返回", got, err)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			check(t, other, domain.UploadStatusUploading, domain.MultipartUploadSessionProtection{}, true, true)
		})
	}
	afterTask, err := store.FindUserUploadTask(ctx, target.UserID, target.TaskID)
	if err != nil || !reflect.DeepEqual(bound, afterTask) {
		t.Fatal("复核改变了已绑定任务", err)
	}
	afterObject, err := store.FindUserStoredObject(ctx, target.UserID, target.ObjectID)
	if err != nil || !reflect.DeepEqual(originalObject, afterObject) || reserved != readReservation() {
		t.Fatal("复核改变了原对象或配额预占", err)
	}

	// 3. 保留终态的绑定。拒绝异常数据和位置已变化的候选。
	execute("UPDATE users SET status='DISABLED' WHERE id=7")
	execute("UPDATE upload_tasks SET status='EXPIRED',reserved_bytes=0,completed_at=UTC_TIMESTAMP(6) WHERE id=19")
	check(t, session, domain.UploadStatusExpired, domain.MultipartUploadSessionProtection{Bound: true}, true, false)
	check(t, other, domain.UploadStatusExpired, domain.MultipartUploadSessionProtection{}, true, true)
	execute("UPDATE stored_objects SET object_key=CONCAT(object_key,' ') WHERE id=23")
	if got, err := store.RecheckMultipartUploadSessionCleanup(ctx, provider, bucket, target, other, grace); got.Eligible() || !errors.Is(err, domain.ErrUploadTaskNotFound) {
		t.Fatal("仍接受定位过期的候选", got, err)
	}
	execute("UPDATE stored_objects SET object_key=? WHERE id=23", key)
	execute("UPDATE upload_tasks SET status='UPLOADING',storage_upload_id=NULL WHERE id=19")
	if got, err := store.RecheckMultipartUploadSessionCleanup(ctx, provider, bucket, target, other, grace); got.Eligible() || !errors.Is(err, domain.ErrInvalidUploadTaskData) {
		t.Fatal("应有绑定的任务缺少会话时被当作安全候选", got, err)
	}
	t.Log("账号、任务、对象三行锁均阻止提前返回，扫描后的租约和绑定获得保护，任务、对象与预占在复核前后一致")
}
