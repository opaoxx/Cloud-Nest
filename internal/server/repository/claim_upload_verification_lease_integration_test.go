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

func TestClaimUploadVerificationLeaseIntegration(t *testing.T) {
	pool, _ := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	execute := func(t *testing.T, statement string) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
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
	scan := func(t *testing.T) domain.UploadVerificationCandidate {
		t.Helper()
		results, err := store.ListUploadVerificationCandidates(ctx, 1)
		if err != nil || len(results) != 1 {
			t.Fatal("未读到待校验候选", results, err)
		}
		return results[0]
	}
	reset := func(t *testing.T) {
		t.Helper()
		execute(t, "UPDATE upload_tasks SET status='VERIFYING',upload_mode='SINGLE',storage_upload_id=NULL,part_size=NULL,part_count=NULL,published_object_id=NULL,resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,reserved_bytes=file_size,lease_owner=NULL,lease_until=NULL,lease_version=3,expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 DAY,next_attempt_at=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND WHERE id=19")
		execute(t, "UPDATE stored_objects SET status='VERIFYING',storage_etag='fixture-etag' WHERE id=23")
	}
	t.Run("concurrent candidates", func(t *testing.T) {
		reset(t)
		before, objectBefore := read(t)
		candidate := scan(t)
		type outcome struct {
			task *domain.UploadTask
			err  error
		}
		start := make(chan struct{})
		outcomes := make(chan outcome, 2)
		for _, owner := range []string{"worker-one", "worker-two"} {
			go func() {
				<-start
				task, err := store.ClaimUploadVerificationLease(ctx, candidate, owner)
				outcomes <- outcome{task, err}
			}()
		}
		close(start)
		var winner *domain.UploadTask
		busy := 0
		for range 2 {
			result := <-outcomes
			switch {
			case result.err == nil && result.task != nil:
				winner = result.task
			case errors.Is(result.err, domain.ErrUploadTaskBusy) && result.task == nil:
				busy++
			default:
				t.Fatal("并发领取发生未知错误", result.task, result.err)
			}
		}
		if winner == nil || busy != 1 || winner.LeaseVersion != 4 || winner.LeaseOwner == nil ||
			winner.LeaseUntil == nil || !winner.LeaseUntil.Equal(winner.UpdatedAt.Add(time.Minute)) {
			t.Fatal("同一候选没有产生唯一租约")
		}
		before.LeaseOwner, before.LeaseUntil, before.LeaseVersion, before.UpdatedAt = winner.LeaseOwner, winner.LeaseUntil, winner.LeaseVersion, winner.UpdatedAt
		after, objectAfter := read(t)
		if !reflect.DeepEqual(after, before) || !reflect.DeepEqual(after, winner) || !reflect.DeepEqual(objectBefore, objectAfter) {
			t.Fatal("领取改变了租约以外的任务或对象记录")
		}
		if next, err := store.ListUploadVerificationCandidates(ctx, 1); err != nil || len(next) != 0 {
			t.Fatal("已领取任务仍在候选中", next, err)
		}
	})
	t.Run("expired lease and maximum version", func(t *testing.T) {
		reset(t)
		execute(t, "UPDATE upload_tasks SET lease_owner='old-worker',lease_until=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND,lease_version=18446744073709551614 WHERE id=19")
		result, err := store.ClaimUploadVerificationLease(ctx, scan(t), "worker")
		if err != nil || result == nil || result.LeaseVersion != math.MaxUint64 || result.LeaseOwner == nil || *result.LeaseOwner != "worker" {
			t.Fatal("已到期租约或最后一个可递增版本未领取", err)
		}
	})
	t.Run("invalid readback rolls back", func(t *testing.T) {
		reset(t)
		before, objectBefore := read(t)
		execute(t, "CREATE TRIGGER fixture_verification_lease BEFORE UPDATE ON upload_tasks FOR EACH ROW SET NEW.lease_owner='fixture-mutated'")
		t.Cleanup(func() { execute(t, "DROP TRIGGER fixture_verification_lease") })
		result, err := store.ClaimUploadVerificationLease(ctx, scan(t), "worker")
		if result != nil || !errors.Is(err, domain.ErrInvalidUploadTaskData) {
			t.Fatal("异常写入结果未拒绝", result, err)
		}
		after, objectAfter := read(t)
		if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(objectBefore, objectAfter) {
			t.Fatal("异常写入未随事务回滚")
		}
	})
}
