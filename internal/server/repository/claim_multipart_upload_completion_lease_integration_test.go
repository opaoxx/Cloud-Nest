package repository

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestClaimMultipartUploadCompletionLeaseIntegration(t *testing.T) {
	ctx, pool, store := prepareMultipartCompletionDatabase(t)
	execute := func(t *testing.T, statement string, arguments ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
			t.Fatal(err)
		}
	}
	const fileSize = domain.MaxSingleUploadSizeBytes + 1
	const session = "Fixture+A/= "
	execute(t, "UPDATE users SET reserved_storage=? WHERE id=7", fileSize)
	execute(t, `INSERT INTO upload_parts(task_id,part_number,etag,size,observed_at) VALUES
		(19,1,'first-etag',?,UTC_TIMESTAMP(6)),(19,2,'tail-etag',1,UTC_TIMESTAMP(6))`, fileSize-1)
	reset := func(t *testing.T) {
		t.Helper()
		execute(t, `UPDATE upload_tasks SET upload_mode='MULTIPART',status='COMPLETING',storage_upload_id=?,
			file_size=?,part_size=?,part_count=2,expected_sha256=UNHEX(REPEAT('ab',32)),reserved_bytes=?,
			published_object_id=NULL,resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,
			lease_owner='prior-worker',lease_until=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND,lease_version=3,
			next_attempt_at=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND,expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR WHERE id=19`,
			session, fileSize, fileSize-1, fileSize)
		execute(t, "UPDATE stored_objects SET status='STAGING',file_size=?,storage_etag=NULL WHERE id=23", fileSize)
	}
	scan := func(t *testing.T) domain.MultipartUploadCompletionCandidate {
		t.Helper()
		candidates, err := store.ListMultipartUploadCompletionCandidates(ctx, 1)
		want := domain.MultipartUploadCompletionCandidate{TaskID: 19, UserID: 7, StagedObjectID: 23, LeaseVersion: 3}
		if err != nil || len(candidates) != 1 || candidates[0] != want {
			t.Fatalf("候选不符: %v %v", candidates, err)
		}
		return candidates[0]
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

	// 1. 两个执行者同时领取同一候选，只提交一份新租约。
	t.Run("one winner", func(t *testing.T) {
		reset(t)
		candidate := scan(t)
		before, objectBefore := read(t)
		type outcome struct {
			task *domain.UploadTask
			err  error
		}
		start := make(chan struct{})
		outcomes := make(chan outcome, 2)
		for _, owner := range []string{"merge-worker-one", "merge-worker-two"} {
			go func() {
				<-start
				task, err := store.ClaimMultipartUploadCompletionLease(ctx, candidate, owner)
				outcomes <- outcome{task: task, err: err}
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
		if winner == nil || busy != 1 || winner.LeaseVersion != 4 || winner.LeaseOwner == nil ||
			(*winner.LeaseOwner != "merge-worker-one" && *winner.LeaseOwner != "merge-worker-two") ||
			winner.LeaseUntil == nil || !winner.LeaseUntil.Equal(winner.UpdatedAt.Add(time.Minute)) ||
			winner.StorageUploadID == nil || *winner.StorageUploadID != session {
			t.Fatal("未产生保持会话原值的唯一 60 秒租约", winner, busy)
		}
		before.LeaseOwner, before.LeaseUntil, before.LeaseVersion, before.UpdatedAt = winner.LeaseOwner, winner.LeaseUntil, winner.LeaseVersion, winner.UpdatedAt
		after, objectAfter := read(t)
		if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(winner, after) || !reflect.DeepEqual(objectBefore, objectAfter) {
			t.Fatal("领取改变了其他任务字段或暂存对象")
		}
		if candidates, err := store.ListMultipartUploadCompletionCandidates(ctx, 1); err != nil || len(candidates) != 0 {
			t.Fatal("已领取任务仍进入候选", candidates, err)
		}
	})

	// 2. 用扫描后的行变化验证领取时复查，并确认拒绝时无写入。
	for _, fixture := range []struct {
		name, change string
		want         error
	}{
		{"phase changed", "UPDATE upload_tasks SET status='UPLOADING' WHERE id=19", domain.ErrUploadTaskNotReady},
		{"session missing", "UPDATE upload_tasks SET storage_upload_id=NULL WHERE id=19", domain.ErrUploadTaskNotReady},
		{"session invalid", "UPDATE upload_tasks SET storage_upload_id='   ' WHERE id=19", domain.ErrInvalidUploadTaskData},
		{"task expired", "UPDATE upload_tasks SET expires_at=UTC_TIMESTAMP(6) WHERE id=19", domain.ErrUploadTaskNotReady},
		{"retry not due", "UPDATE upload_tasks SET next_attempt_at=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR WHERE id=19", domain.ErrUploadTaskNotReady},
		{"lease still active", "UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR WHERE id=19", domain.ErrUploadTaskBusy},
		{"stale candidate", "UPDATE upload_tasks SET lease_version=4 WHERE id=19", domain.ErrUploadTaskBusy},
		{"invalid plan", "UPDATE upload_tasks SET part_count=3 WHERE id=19", domain.ErrInvalidUploadTaskData},
		{"missing hash", "UPDATE upload_tasks SET expected_sha256=NULL WHERE id=19", domain.ErrInvalidUploadTaskData},
		{"reservation changed", "UPDATE upload_tasks SET reserved_bytes=0 WHERE id=19", domain.ErrInvalidUploadTaskData},
		{"object moved on", "UPDATE stored_objects SET status='ORPHANED' WHERE id=23", domain.ErrInvalidStoredObjectData},
		{"object has result metadata", "UPDATE stored_objects SET storage_etag='written' WHERE id=23", domain.ErrInvalidStoredObjectData},
		{"disabled account", "UPDATE users SET status='DISABLED' WHERE id=7", domain.ErrAccountDisabled},
		{"wrong account", "", domain.ErrUploadTaskNotFound},
		{"wrong staged object", "", domain.ErrUploadTaskBusy},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			reset(t)
			candidate := scan(t)
			if fixture.change != "" {
				execute(t, fixture.change)
			}
			switch fixture.name {
			case "wrong account":
				candidate.UserID = 8
			case "wrong staged object":
				candidate.StagedObjectID++
			}
			if fixture.name == "disabled account" {
				t.Cleanup(func() { execute(t, "UPDATE users SET status='ACTIVE' WHERE id=7") })
			}
			before, objectBefore := read(t)
			result, err := store.ClaimMultipartUploadCompletionLease(ctx, candidate, "merge-worker")
			if result != nil || !errors.Is(err, fixture.want) {
				t.Fatalf("预期拒绝 %v，得到 %+v %v", fixture.want, result, err)
			}
			after, objectAfter := read(t)
			if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(objectBefore, objectAfter) {
				t.Fatal("失效候选改变了任务或暂存对象")
			}
		})
	}

	// 3. 模拟写后会话原值变化，验证回读拒绝且整次写入回滚。
	t.Run("session changed on write", func(t *testing.T) {
		reset(t)
		candidate := scan(t)
		before, objectBefore := read(t)
		execute(t, "CREATE TRIGGER fixture_merge_lease BEFORE UPDATE ON upload_tasks FOR EACH ROW SET NEW.storage_upload_id=RTRIM(NEW.storage_upload_id)")
		t.Cleanup(func() { execute(t, "DROP TRIGGER fixture_merge_lease") })
		result, err := store.ClaimMultipartUploadCompletionLease(ctx, candidate, "merge-worker")
		if result != nil || !errors.Is(err, domain.ErrInvalidUploadTaskData) {
			t.Fatal("写后会话原值变化未被拒绝", result, err)
		}
		after, objectAfter := read(t)
		if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(objectBefore, objectAfter) {
			t.Fatal("异常写入未完整回滚")
		}
	})

	// 4. 核对正式回执和账号配额保持原值。
	var reserved, used uint64
	if err := pool.QueryRowContext(ctx, "SELECT reserved_storage,used_storage FROM users WHERE id=7").Scan(&reserved, &used); err != nil || reserved != fileSize || used != 0 {
		t.Fatal("领取改变了账号空间结算", reserved, used, err)
	}
	var total, matching int
	if err := pool.QueryRowContext(ctx, `SELECT COUNT(*),SUM((part_number=1 AND size=? AND etag='first-etag') OR
		(part_number=2 AND size=1 AND etag='tail-etag')) FROM upload_parts WHERE task_id=19`, fileSize-1).Scan(&total, &matching); err != nil || total != 2 || matching != 2 {
		t.Fatal("领取改变了已冻结的两条回执", total, matching, err)
	}
}
