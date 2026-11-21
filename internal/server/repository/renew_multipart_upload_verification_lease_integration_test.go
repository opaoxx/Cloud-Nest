package repository

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestRenewMultipartUploadVerificationLeaseIntegration(t *testing.T) {
	ctx, pool, store := prepareMultipartCompletionDatabase(t)
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	const fileSize = domain.MaxSingleUploadSizeBytes + 1
	const session, worker = "Fixture+A/= ", "fixture-verification-worker"
	start := time.Now().UTC().Truncate(time.Second)
	execute := func(t *testing.T, statement string, arguments ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
			t.Fatal(err)
		}
	}
	clock := func(t *testing.T, elapsed time.Duration) {
		t.Helper()
		execute(t, "SET timestamp = ?", start.Add(elapsed).Unix())
	}
	t.Cleanup(func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := pool.ExecContext(cleanupContext, "SET timestamp = DEFAULT"); err != nil {
			t.Error("复原独占数据库连接时钟失败", err)
		}
	})
	execute(t, "UPDATE users SET used_storage=1024,reserved_storage=? WHERE id=7", fileSize+512)
	execute(t, `UPDATE stored_objects SET status='VERIFYING',file_size=?,storage_etag='fixture-etag',
		storage_version_id='fixture-version' WHERE id=23`, fileSize)
	read := func(t *testing.T) (*domain.UploadTask, *domain.StoredObject) {
		t.Helper()
		task, err := store.FindUserUploadTask(ctx, 7, 19)
		if err != nil {
			t.Fatal(err)
		}
		object, err := store.FindUserStoredObject(ctx, 7, 23)
		if err != nil {
			t.Fatal(err)
		}
		return task, object
	}
	claim := func(t *testing.T, expirySeconds int, priorVersion uint64) *domain.UploadTask {
		t.Helper()
		// 1. 通过正式扫描和领取提交 60 秒初始租约。
		clock(t, 0)
		execute(t, `UPDATE upload_tasks SET status='VERIFYING',upload_mode='MULTIPART',storage_upload_id=?,
			file_size=?,part_size=?,part_count=2,expected_sha256=UNHEX(REPEAT('ab',32)),reserved_bytes=?,
			published_object_id=NULL,resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,
			lease_owner=NULL,lease_until=NULL,lease_version=?,next_attempt_at=NULL,
			expires_at=TIMESTAMPADD(SECOND, ?, UTC_TIMESTAMP(6)) WHERE id=19`, session, fileSize, fileSize-1, fileSize, priorVersion, expirySeconds)
		candidates, err := store.ListMultipartUploadVerificationCandidates(ctx, 1)
		if err != nil || len(candidates) != 1 {
			t.Fatal("未扫描到 Multipart 校验候选", candidates, err)
		}
		task, err := store.ClaimMultipartUploadVerificationLease(ctx, candidates[0], worker)
		if err != nil || task == nil || task.LeaseVersion != priorVersion+1 || task.LeaseUntil == nil || !task.LeaseUntil.Equal(start.Add(time.Minute)) {
			t.Fatal("未领取初始 60 秒租约", task, err)
		}
		return task
	}
	assertRenewed := func(t *testing.T, original *domain.UploadTask, objectBefore *domain.StoredObject, elapsed time.Duration) *domain.UploadTask {
		t.Helper()
		clock(t, elapsed)
		renewed, err := store.RenewMultipartUploadVerificationLease(ctx, original)
		if err != nil || renewed == nil || renewed.LeaseUntil == nil || !renewed.LeaseUntil.Equal(start.Add(elapsed+time.Minute)) ||
			!renewed.UpdatedAt.Equal(start.Add(elapsed)) {
			t.Fatal("续租期限或结果错误", renewed, err)
		}
		expected := *original
		expected.LeaseUntil, expected.UpdatedAt = renewed.LeaseUntil, renewed.UpdatedAt
		persisted, object := read(t)
		if !reflect.DeepEqual(renewed, &expected) || !reflect.DeepEqual(renewed, persisted) || !reflect.DeepEqual(object, objectBefore) {
			t.Fatal("续租改变了期限以外的任务数据，或改变了对象")
		}
		return renewed
	}

	// 2. 用原领取快照续约两次，并在原租约到期后进入实际校验写回复核。
	t.Run("extend using original snapshot and write back after initial deadline", func(t *testing.T) {
		original := claim(t, 3600, 3)
		_, object := read(t)
		assertRenewed(t, original, object, 0)
		assertRenewed(t, original, object, 20*time.Second)
		assertRenewed(t, original, object, 20*time.Second)
		renewed := assertRenewed(t, original, object, 40*time.Second)
		clock(t, 60*time.Second)
		if candidates, err := store.ListMultipartUploadVerificationCandidates(ctx, 1); err != nil || len(candidates) != 0 {
			t.Fatal("已续约任务在初始截止时间被重新扫描", candidates, err)
		}
		calls := 0
		digest := domain.UploadContentDigest{SizeBytes: fileSize, SHA256: *original.ExpectedSHA256}
		err := store.WithUploadVerificationTransaction(ctx, original, object, digest,
			func(_ context.Context, _ *Store, current *domain.UploadTask, staged *domain.StoredObject) error {
				calls++
				if !reflect.DeepEqual(current, renewed) || !reflect.DeepEqual(staged, object) {
					t.Fatal("写回复核未使用续约后的当前任务及原对象")
				}
				return nil
			})
		if err != nil || calls != 1 {
			t.Fatal("原租约到期后仍有效的续约未通过写回复核", calls, err)
		}
		t.Log("任务 19 的租约版本保持 4，数据库截止时间依次为起点后 60、80、100 秒")
	})
	t.Run("retain longer deadline", func(t *testing.T) {
		original := claim(t, 3600, 3)
		execute(t, "UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6)+INTERVAL 2 MINUTE WHERE id=19")
		before, _ := read(t)
		clock(t, 20*time.Second)
		result, err := store.RenewMultipartUploadVerificationLease(ctx, original)
		after, _ := read(t)
		if err != nil || !reflect.DeepEqual(result, before) || !reflect.DeepEqual(after, before) {
			t.Fatal("已更长的租约被改写", result, err)
		}
	})
	t.Run("maximum version still renews", func(t *testing.T) {
		original := claim(t, 3600, math.MaxUint64-1)
		_, object := read(t)
		assertRenewed(t, original, object, 20*time.Second)
	})
	t.Run("new claimant fences old lease", func(t *testing.T) {
		original := claim(t, 3600, 3)
		clock(t, 60*time.Second)
		candidates, err := store.ListMultipartUploadVerificationCandidates(ctx, 1)
		if err != nil || len(candidates) != 1 {
			t.Fatal("到期任务未重新进入候选", candidates, err)
		}
		current, err := store.ClaimMultipartUploadVerificationLease(ctx, candidates[0], "fixture-replacement-worker")
		if err != nil || current == nil || current.LeaseVersion != original.LeaseVersion+1 {
			t.Fatal("未提交新一代租约", current, err)
		}
		if result, err := store.RenewMultipartUploadVerificationLease(ctx, original); result != nil || !errors.Is(err, domain.ErrUploadTaskBusy) {
			t.Fatal("被替换的持有者仍能续约", result, err)
		}
		persisted, _ := read(t)
		if !reflect.DeepEqual(persisted, current) {
			t.Fatal("旧租约改变了新持有者的任务")
		}
	})

	// 3. 检查过期、快照变化及回读异常均保留原数据库状态。
	for _, fixture := range []struct {
		name, change string
		advance      time.Duration
		expiry       int
		cancel       bool
		alterClaim   func(*domain.UploadTask)
		want         error
	}{
		{name: "lease exactly expired", advance: time.Minute, want: domain.ErrUploadTaskBusy},
		{name: "task exactly expired", advance: 30 * time.Second, expiry: 30, want: domain.ErrUploadTaskNotReady},
		{name: "owner case differs", alterClaim: func(task *domain.UploadTask) { task.LeaseOwner = new("Fixture-verification-worker") }, want: domain.ErrUploadTaskBusy},
		{name: "old version", alterClaim: func(task *domain.UploadTask) { task.LeaseVersion-- }, want: domain.ErrUploadTaskBusy},
		{name: "other account", alterClaim: func(task *domain.UploadTask) { task.UserID = 8 }, want: domain.ErrUploadTaskNotFound},
		{name: "changed storage session", change: "UPDATE upload_tasks SET storage_upload_id=RTRIM(storage_upload_id) WHERE id=19", want: domain.ErrUploadTaskBusy},
		{name: "changed part declaration", change: "UPDATE upload_tasks SET part_size=part_size-1 WHERE id=19", want: domain.ErrInvalidUploadTaskData},
		{name: "incomplete reservation", change: "UPDATE upload_tasks SET reserved_bytes=file_size-1 WHERE id=19", want: domain.ErrInvalidUploadTaskData},
		{name: "stage changed", change: "UPDATE upload_tasks SET status='CANCELLING' WHERE id=19", want: domain.ErrUploadTaskNotReady},
		{name: "cancelled context", cancel: true, want: context.Canceled},
		{name: "rollback invalid readback", advance: 20 * time.Second,
			change: "CREATE TRIGGER fixture_renew BEFORE UPDATE ON upload_tasks FOR EACH ROW SET NEW.storage_upload_id=RTRIM(OLD.storage_upload_id)",
			want:   domain.ErrInvalidUploadTaskData},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			expiry := fixture.expiry
			if expiry == 0 {
				expiry = 3600
			}
			original := claim(t, expiry, 3)
			clock(t, fixture.advance)
			if fixture.change != "" {
				execute(t, fixture.change)
			}
			if fixture.name == "rollback invalid readback" {
				t.Cleanup(func() { execute(t, "DROP TRIGGER fixture_renew") })
			}
			if fixture.alterClaim != nil {
				fixture.alterClaim(original)
			}
			before, objectBefore := read(t)
			renewContext, cancel := context.WithCancel(ctx)
			defer cancel()
			if fixture.cancel {
				cancel()
			}
			if result, err := store.RenewMultipartUploadVerificationLease(renewContext, original); result != nil || !errors.Is(err, fixture.want) {
				t.Fatal("续租未按预期拒绝", result, err)
			}
			after, objectAfter := read(t)
			if !reflect.DeepEqual(after, before) || !reflect.DeepEqual(objectAfter, objectBefore) {
				t.Fatal("拒绝的续租改变了任务或对象")
			}
		})
	}
	var used, reserved uint64
	if err := pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage FROM users WHERE id=7").Scan(&used, &reserved); err != nil ||
		used != 1024 || reserved != fileSize+512 {
		t.Fatal("续租改变了用户配额", used, reserved, err)
	}
}
