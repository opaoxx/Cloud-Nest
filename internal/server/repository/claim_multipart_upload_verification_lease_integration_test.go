package repository

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestClaimMultipartUploadVerificationLeaseIntegration(t *testing.T) {
	ctx, pool, store := prepareMultipartCompletionDatabase(t)
	const size = domain.MaxSingleUploadSizeBytes + 1
	const session = "Fixture+A/= "
	execute := func(t *testing.T, statement string, arguments ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
			t.Fatal(err)
		}
	}
	// 1. 准备与合并写回一致的待校验任务及对象。
	execute(t, "UPDATE users SET reserved_storage=? WHERE id=7", size)
	reset := func(t *testing.T) {
		t.Helper()
		execute(t, `UPDATE upload_tasks SET status='VERIFYING',upload_mode='MULTIPART',storage_upload_id=?,
			file_size=?,part_size=?,part_count=2,expected_sha256=UNHEX(REPEAT('ab',32)),reserved_bytes=?,
			published_object_id=NULL,resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,
			lease_owner='old-worker',lease_until=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND,lease_version=3,
			next_attempt_at=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND,expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR WHERE id=19`,
			session, size, size-1, size)
		execute(t, "UPDATE stored_objects SET status='VERIFYING',file_size=?,storage_etag='fixture-etag',storage_version_id='fixture-version' WHERE id=23", size)
	}
	scan := func(t *testing.T) domain.UploadVerificationCandidate {
		t.Helper()
		results, err := store.ListMultipartUploadVerificationCandidates(ctx, 1)
		want := domain.UploadVerificationCandidate{TaskID: 19, UserID: 7, StagedObjectID: 23, LeaseVersion: 3}
		if err != nil || len(results) != 1 || results[0] != want {
			t.Fatal("未读到预期的分片校验候选", results, err)
		}
		return results[0]
	}
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

	// 2. 并发领取同一候选，验证只提交一份租约且对象定位原样保留。
	t.Run("concurrent claim", func(t *testing.T) {
		reset(t)
		candidate := scan(t)
		before, objectBefore := read(t)
		type outcome struct {
			task *domain.UploadTask
			err  error
		}
		start := make(chan struct{})
		outcomes := make(chan outcome, 2)
		for _, owner := range []string{"verification-one", "verification-two"} {
			go func() {
				<-start
				task, err := store.ClaimMultipartUploadVerificationLease(ctx, candidate, owner)
				outcomes <- outcome{task: task, err: err}
			}()
		}
		close(start)
		var winner *domain.UploadTask
		busy := 0
		for range 2 {
			result := <-outcomes
			switch {
			case result.task != nil && result.err == nil:
				winner = result.task
			case result.task == nil && errors.Is(result.err, domain.ErrUploadTaskBusy):
				busy++
			default:
				t.Fatal("领取竞争结果异常", result.err)
			}
		}
		if winner == nil || busy != 1 || winner.LeaseVersion != 4 || winner.LeaseOwner == nil ||
			winner.LeaseUntil == nil || !winner.LeaseUntil.Equal(winner.UpdatedAt.Add(time.Minute)) {
			t.Fatal("未提交唯一的 60 秒租约", busy)
		}
		before.LeaseOwner, before.LeaseUntil, before.LeaseVersion, before.UpdatedAt = winner.LeaseOwner, winner.LeaseUntil, winner.LeaseVersion, winner.UpdatedAt
		after, objectAfter := read(t)
		if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(winner, after) || !reflect.DeepEqual(objectBefore, objectAfter) {
			t.Fatal("领取改变了其他任务字段或对象定位")
		}
		if next, err := store.ListMultipartUploadVerificationCandidates(ctx, 1); err != nil || len(next) != 0 {
			t.Fatal("持有有效租约的任务仍进入候选", next, err)
		}
		t.Log("任务 19 的两个执行者只提交一份 60 秒租约，版本 3 → 4")
	})

	// 3. 模拟扫描后变化与异常写回，核对失败不留下租约改动。
	for _, fixture := range []struct {
		name, change string
		want         error
	}{
		{"status changed", "UPDATE upload_tasks SET status='CANCELLING' WHERE id=19", domain.ErrUploadTaskNotReady},
		{"session cleared", "UPDATE upload_tasks SET storage_upload_id=NULL WHERE id=19", domain.ErrUploadTaskNotReady},
		{"invalid session", "UPDATE upload_tasks SET storage_upload_id='   ' WHERE id=19", domain.ErrInvalidUploadTaskData},
		{"task due", "UPDATE upload_tasks SET expires_at=UTC_TIMESTAMP(6) WHERE id=19", domain.ErrUploadTaskNotReady},
		{"retry postponed", "UPDATE upload_tasks SET next_attempt_at=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR WHERE id=19", domain.ErrUploadTaskNotReady},
		{"active lease", "UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR WHERE id=19", domain.ErrUploadTaskBusy},
		{"stale version", "UPDATE upload_tasks SET lease_version=4 WHERE id=19", domain.ErrUploadTaskBusy},
		{"missing checksum", "UPDATE upload_tasks SET expected_sha256=NULL WHERE id=19", domain.ErrInvalidUploadTaskData},
		{"invalid plan", "UPDATE upload_tasks SET part_count=3 WHERE id=19", domain.ErrInvalidUploadTaskData},
		{"object not verifying", "UPDATE stored_objects SET status='STAGING' WHERE id=23", domain.ErrInvalidStoredObjectData},
		{"object ETag missing", "UPDATE stored_objects SET storage_etag=NULL WHERE id=23", domain.ErrInvalidStoredObjectData},
		{"object version blank", "UPDATE stored_objects SET storage_version_id=' ' WHERE id=23", domain.ErrInvalidStoredObjectData},
		{"foreign owner", "", domain.ErrUploadTaskNotFound},
		{"session modified by trigger", "CREATE TRIGGER fixture_verify_lease BEFORE UPDATE ON upload_tasks FOR EACH ROW SET NEW.storage_upload_id=RTRIM(NEW.storage_upload_id)", domain.ErrInvalidUploadTaskData},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			reset(t)
			candidate := scan(t)
			if fixture.change != "" {
				execute(t, fixture.change)
			} else {
				candidate.UserID = 8
			}
			if fixture.name == "session modified by trigger" {
				t.Cleanup(func() { execute(t, "DROP TRIGGER fixture_verify_lease") })
			}
			before, objectBefore := read(t)
			if got, err := store.ClaimMultipartUploadVerificationLease(ctx, candidate, "verification-worker"); got != nil || !errors.Is(err, fixture.want) {
				t.Fatal("未按预期拒绝领取", fixture.want, err)
			}
			after, objectAfter := read(t)
			if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(objectBefore, objectAfter) {
				t.Fatal("失败后任务或对象未保持原值")
			}
		})
	}
	t.Run("optional version remains absent", func(t *testing.T) {
		reset(t)
		execute(t, "UPDATE stored_objects SET storage_version_id=NULL WHERE id=23")
		if got, err := store.ClaimMultipartUploadVerificationLease(ctx, scan(t), "verification-worker"); err != nil || got == nil || got.LeaseVersion != 4 {
			t.Fatal("缺省版本的合并对象未领取租约", err)
		}
		_, object := read(t)
		if object.StorageVersionID != nil || object.StorageETag == nil || *object.StorageETag != "fixture-etag" {
			t.Fatal("缺省版本的对象定位被更改")
		}
	})
	var reserved, used uint64
	if err := pool.QueryRowContext(ctx, "SELECT reserved_storage,used_storage FROM users WHERE id=7").Scan(&reserved, &used); err != nil || reserved != size || used != 0 {
		t.Fatal("领取之后账号配额不符", reserved, used, err)
	}
}
