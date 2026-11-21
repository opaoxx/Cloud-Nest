package repository

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestClaimMultipartUploadInitializationLeaseIntegration(t *testing.T) {
	pool, _ := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
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
	scan := func(t *testing.T) domain.MultipartUploadInitializationCandidate {
		t.Helper()
		items, err := store.ListMultipartUploadInitializationCandidates(ctx, 1)
		if err != nil || len(items) != 1 {
			t.Fatalf("预期一条初始化候选: %v %v", items, err)
		}
		return items[0]
	}
	reset := func(t *testing.T) {
		t.Helper()
		execute(t, `UPDATE upload_tasks SET status='CREATING', storage_upload_id=NULL, published_object_id=NULL, resolved_name=NULL,
			result_entry_id=NULL, completed_at=NULL, last_error_code=NULL, last_error_message=NULL,
			file_size=16777217, part_size=16777216, part_count=2, reserved_bytes=16777217,
			lease_owner=NULL, lease_until=NULL, lease_version=3, next_attempt_at=NULL,
			expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 DAY WHERE id=19`)
		execute(t, "UPDATE stored_objects SET status='STAGING',file_size=16777217 WHERE id=23")
	}

	// 1. 两个 worker 同时用一份候选领取，只允许一个新持有者。
	t.Run("concurrent claim", func(t *testing.T) {
		reset(t)
		before, objectBefore := read(t)
		candidate := scan(t)
		type outcome struct {
			task *domain.UploadTask
			err  error
		}
		start := make(chan struct{})
		outcomes := make(chan outcome, 2)
		for _, owner := range []string{"fixture-one", "fixture-two"} {
			go func() {
				<-start
				task, err := store.ClaimMultipartUploadInitializationLease(ctx, candidate, owner)
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
				t.Fatalf("并发领取结果异常: %+v %v", result.task, result.err)
			}
		}
		if winner == nil || busy != 1 || winner.LeaseVersion != 4 ||
			winner.LeaseOwner == nil || winner.LeaseUntil == nil || !winner.LeaseUntil.Equal(winner.UpdatedAt.Add(time.Minute)) {
			t.Fatal("未产生唯一的 60 秒租约", winner, busy)
		}
		before.LeaseOwner, before.LeaseUntil, before.LeaseVersion, before.UpdatedAt = winner.LeaseOwner, winner.LeaseUntil, winner.LeaseVersion, winner.UpdatedAt
		after, objectAfter := read(t)
		if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(winner, after) || !reflect.DeepEqual(objectBefore, objectAfter) {
			t.Fatal("领取改变了其他任务字段或暂存对象")
		}
		if got, err := store.ListMultipartUploadInitializationCandidates(ctx, 1); err != nil || len(got) != 0 {
			t.Fatal("已领取任务仍进入候选", got, err)
		}
	})

	// 2. 扫描后的状态变化必须在领取事务中重新判断。
	for _, fixture := range []struct {
		name, change string
		want         error
	}{
		{"session already bound", "UPDATE upload_tasks SET storage_upload_id='fixture-bound' WHERE id=19", domain.ErrUploadTaskNotReady},
		{"task expired", "UPDATE upload_tasks SET expires_at=UTC_TIMESTAMP(6) WHERE id=19", domain.ErrUploadTaskNotReady},
		{"stale lease version", "UPDATE upload_tasks SET lease_version=4 WHERE id=19", domain.ErrUploadTaskBusy},
		{"invalid plan", "UPDATE upload_tasks SET part_count=3 WHERE id=19", domain.ErrInvalidUploadTaskData},
		{"object moved on", "UPDATE stored_objects SET status='ORPHANED' WHERE id=23", domain.ErrInvalidStoredObjectData},
		{"disabled account", "UPDATE users SET status='DISABLED' WHERE id=7", domain.ErrAccountDisabled},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			reset(t)
			candidate := scan(t)
			execute(t, fixture.change)
			before, objectBefore := read(t)
			result, err := store.ClaimMultipartUploadInitializationLease(ctx, candidate, "fixture-worker")
			if result != nil || !errors.Is(err, fixture.want) {
				t.Fatalf("预期拒绝 %v，得到 %+v %v", fixture.want, result, err)
			}
			after, objectAfter := read(t)
			if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(objectBefore, objectAfter) {
				t.Fatal("失效候选改变了任务或暂存对象")
			}
			execute(t, "UPDATE users SET status='ACTIVE' WHERE id=7")
		})
	}

	// 3. 注入写后计划变化，验证校验失败导致租约写入回滚。
	t.Run("invalid readback rolls back", func(t *testing.T) {
		reset(t)
		candidate := scan(t)
		before, objectBefore := read(t)
		execute(t, "CREATE TRIGGER fixture_initialization_lease BEFORE UPDATE ON upload_tasks FOR EACH ROW SET NEW.part_count=3")
		t.Cleanup(func() { execute(t, "DROP TRIGGER fixture_initialization_lease") })
		result, err := store.ClaimMultipartUploadInitializationLease(ctx, candidate, "fixture-worker")
		if result != nil || !errors.Is(err, domain.ErrInvalidUploadTaskData) {
			t.Fatal("写后异常分片计划未拒绝", result, err)
		}
		after, objectAfter := read(t)
		if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(objectBefore, objectAfter) {
			t.Fatal("异常写入未完整回滚")
		}
	})
}
