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

func TestBindMultipartUploadSessionIntegration(t *testing.T) {
	pool, _ := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	const owner = "fixture-worker"
	execute := func(t *testing.T, statement string) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	claim := func(t *testing.T) *domain.UploadTask {
		t.Helper()
		candidates, err := store.ListMultipartUploadInitializationCandidates(ctx, 1)
		if err != nil || len(candidates) != 1 {
			t.Fatal("预期一条可领取候选", candidates, err)
		}
		claimed, err := store.ClaimMultipartUploadInitializationLease(ctx, candidates[0], owner)
		if err != nil {
			t.Fatal(err)
		}
		return claimed
	}
	resetAndClaim := func(t *testing.T) *domain.UploadTask {
		t.Helper()
		execute(t, `UPDATE upload_tasks SET status='CREATING',storage_upload_id=NULL,published_object_id=NULL,
			resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,file_size=16777217,part_size=16777216,
			part_count=2,reserved_bytes=16777217,lease_owner=NULL,lease_until=NULL,lease_version=3,
			next_attempt_at=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND,attempt_count=2,
			last_error_code='STORAGE_UNAVAILABLE',last_error_message='fixture retry',
			expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 DAY WHERE id=19`)
		execute(t, "UPDATE stored_objects SET status='STAGING',file_size=16777217 WHERE id=23")
		execute(t, "UPDATE users SET reserved_storage=16777217,used_storage=16777216,status='ACTIVE' WHERE id=7")
		return claim(t)
	}
	type snapshot struct {
		task     *domain.UploadTask
		object   *domain.StoredObject
		reserved uint64
		used     uint64
	}
	read := func(t *testing.T) snapshot {
		t.Helper()
		task, err := store.FindUserUploadTask(ctx, 7, 19)
		if err != nil {
			t.Fatal(err)
		}
		object, err := store.FindUserStoredObject(ctx, 7, 23)
		if err != nil {
			t.Fatal(err)
		}
		result := snapshot{task: task, object: object}
		err = pool.QueryRowContext(ctx, "SELECT reserved_storage,used_storage FROM users WHERE id=7").Scan(&result.reserved, &result.used)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	checkRejected := func(t *testing.T, claimed *domain.UploadTask, uploadID string, want error) {
		t.Helper()
		before := read(t)
		bound, err := store.BindMultipartUploadSession(ctx, claimed, uploadID)
		if bound != nil || !errors.Is(err, want) {
			t.Fatalf("预期拒绝 %v，得到 %+v %v", want, bound, err)
		}
		if !reflect.DeepEqual(before, read(t)) {
			t.Fatal("拒绝绑定后任务、对象或配额发生变化")
		}
	}

	// 1. 从真实领租结果绑定会话，核对提交及重复请求。
	t.Run("bind and preserve reservation", func(t *testing.T) {
		claimed := resetAndClaim(t)
		before := read(t)
		uploadID := " " + strings.Repeat("a", 1020) + "/+ "
		bound, err := store.BindMultipartUploadSession(ctx, claimed, uploadID)
		if err != nil {
			t.Fatal(err)
		}
		want := *before.task
		want.Status, want.StorageUploadID = domain.UploadStatusUploading, &uploadID
		want.LeaseOwner, want.LeaseUntil, want.NextAttemptAt = nil, nil, nil
		want.LastErrorCode, want.LastErrorMessage, want.UpdatedAt = nil, nil, bound.UpdatedAt
		if !reflect.DeepEqual(&want, bound) {
			t.Fatal("绑定未原样保存会话，或修改了预期之外的任务字段")
		}
		before.task = &want
		if !reflect.DeepEqual(before, read(t)) {
			t.Fatal("提交后的任务、对象或配额与预期不符")
		}
		checkRejected(t, claimed, uploadID, domain.ErrUploadTaskNotReady)
		checkRejected(t, claimed, "fixture-other-session", domain.ErrUploadTaskNotReady)
	})

	// 2. 过期和重新领取后，旧 worker 即使持有相同标识也不能覆盖当前租约。
	t.Run("expired and superseded lease", func(t *testing.T) {
		old := resetAndClaim(t)
		execute(t, "UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6) WHERE id=19")
		checkRejected(t, old, "fixture-late", domain.ErrUploadTaskBusy)
		current := claim(t)
		if *old.LeaseOwner != *current.LeaseOwner || current.LeaseVersion != old.LeaseVersion+1 {
			t.Fatal("夹具未用相同持有者取得新版本")
		}
		checkRejected(t, old, "fixture-late", domain.ErrUploadTaskBusy)
		if _, err := store.BindMultipartUploadSession(ctx, current, "fixture-current"); err != nil {
			t.Fatal(err)
		}
	})

	for _, fixture := range []struct {
		name, change string
		want         error
	}{
		{"different lease owner", "UPDATE upload_tasks SET lease_owner='fixture-other' WHERE id=19", domain.ErrUploadTaskBusy},
		{"task expired", "UPDATE upload_tasks SET expires_at=UTC_TIMESTAMP(6) WHERE id=19", domain.ErrUploadTaskNotReady},
		{"invalid part plan", "UPDATE upload_tasks SET part_count=3 WHERE id=19", domain.ErrInvalidUploadTaskData},
		{"object moved on", "UPDATE stored_objects SET status='ORPHANED' WHERE id=23", domain.ErrInvalidStoredObjectData},
		{"account disabled", "UPDATE users SET status='DISABLED' WHERE id=7", domain.ErrAccountDisabled},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			claimed := resetAndClaim(t)
			execute(t, fixture.change)
			checkRejected(t, claimed, "fixture-session", fixture.want)
		})
	}
	t.Run("wrong object or invalid session", func(t *testing.T) {
		claimed := resetAndClaim(t)
		for _, invalid := range []string{" ", "会话", strings.Repeat("a", 1025)} {
			checkRejected(t, claimed, invalid, domain.ErrInvalidUploadTaskData)
		}
		claimed.StagedObjectID++
		checkRejected(t, claimed, "fixture-session", domain.ErrUploadTaskBusy)
	})

	// 3. 两个写回也只允许保存一个会话，异常回读回滚整个写入。
	t.Run("concurrent bind", func(t *testing.T) {
		claimed := resetAndClaim(t)
		type outcome struct {
			id    string
			bound *domain.UploadTask
			err   error
		}
		start := make(chan struct{})
		outcomes := make(chan outcome, 2)
		for _, uploadID := range []string{"fixture-first", "fixture-second"} {
			go func() {
				<-start
				bound, err := store.BindMultipartUploadSession(ctx, claimed, uploadID)
				outcomes <- outcome{uploadID, bound, err}
			}()
		}
		close(start)
		var winner *domain.UploadTask
		rejected := 0
		for range 2 {
			result := <-outcomes
			switch {
			case result.err == nil && result.bound != nil && result.bound.StorageUploadID != nil && *result.bound.StorageUploadID == result.id:
				winner = result.bound
			case result.bound == nil && errors.Is(result.err, domain.ErrUploadTaskNotReady):
				rejected++
			default:
				t.Fatalf("并发绑定结果异常: %+v %v", result.bound, result.err)
			}
		}
		if rejected != 1 || winner == nil || !reflect.DeepEqual(winner, read(t).task) {
			t.Fatal("并发写入未保留唯一成功的会话")
		}
	})
	t.Run("invalid readback rolls back", func(t *testing.T) {
		claimed := resetAndClaim(t)
		execute(t, "CREATE TRIGGER fixture_bind_session BEFORE UPDATE ON upload_tasks FOR EACH ROW SET NEW.part_count=3")
		t.Cleanup(func() { execute(t, "DROP TRIGGER fixture_bind_session") })
		checkRejected(t, claimed, "fixture-session", domain.ErrInvalidUploadTaskData)
	})
}
