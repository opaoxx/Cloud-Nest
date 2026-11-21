package repository

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestClaimMultipartUploadCancellationLeaseIntegration(t *testing.T) {
	ctx, pool, store := prepareMultipartCompletionDatabase(t)
	execute := func(t *testing.T, statement string, arguments ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
			t.Fatal(err)
		}
	}
	scan := func(t *testing.T) domain.UploadCancellationCandidate {
		t.Helper()
		candidates, err := store.ListMultipartUploadCancellationCandidates(ctx, 1)
		if err != nil || len(candidates) != 1 || candidates[0].UserID != 7 || candidates[0].TaskID != 19 || candidates[0].StagedObjectID != 23 {
			t.Fatal("未取得待领取的取消候选", candidates, err)
		}
		return candidates[0]
	}
	type facts struct {
		task           *domain.UploadTask
		object         *domain.StoredObject
		used, reserved uint64
	}
	read := func(t *testing.T) facts {
		t.Helper()
		task, err := store.FindUserUploadTask(ctx, 7, 19)
		if err != nil {
			t.Fatal(err)
		}
		object, err := store.FindUserStoredObject(ctx, 7, 23)
		if err != nil {
			t.Fatal(err)
		}
		result := facts{task: task, object: object}
		if err := pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage FROM users WHERE id=7").Scan(&result.used, &result.reserved); err != nil {
			t.Fatal(err)
		}
		return result
	}
	assertClaimed := func(t *testing.T, before facts, claimed *domain.UploadTask, owner string) {
		t.Helper()
		if claimed == nil || claimed.LeaseOwner == nil || *claimed.LeaseOwner != owner || claimed.LeaseVersion != before.task.LeaseVersion+1 ||
			claimed.LeaseUntil == nil || !claimed.LeaseUntil.Equal(claimed.UpdatedAt.Add(time.Minute)) {
			t.Fatal("未正确领取 60 秒取消收尾租约", claimed)
		}
		expected := *before.task
		expected.LeaseOwner, expected.LeaseUntil = claimed.LeaseOwner, claimed.LeaseUntil
		expected.LeaseVersion, expected.UpdatedAt = claimed.LeaseVersion, claimed.UpdatedAt
		before.task = &expected
		if !reflect.DeepEqual(claimed, &expected) || !reflect.DeepEqual(read(t), before) {
			t.Fatal("领取改变了会话、对象、预占或其他非租约字段", claimed)
		}
	}

	// 1. 两个真实事务争用同一候选，最多提交一个新版本。
	t.Run("concurrent claim", func(t *testing.T) {
		resetMultipartUploadCancellationFixture(t, ctx, pool)
		execute(t, "UPDATE stored_objects SET status='VERIFYING',storage_etag='fixture-etag',storage_version_id='fixture-version' WHERE id=23")
		candidate, before := scan(t), read(t)
		type outcome struct {
			owner string
			task  *domain.UploadTask
			err   error
		}
		start, results := make(chan struct{}), make(chan outcome, 2)
		for _, owner := range []string{"fixture-cleanup-one", "fixture-cleanup-two"} {
			go func() {
				<-start
				task, err := store.ClaimMultipartUploadCancellationLease(ctx, candidate, owner)
				results <- outcome{owner: owner, task: task, err: err}
			}()
		}
		close(start)
		winner, loser := <-results, <-results
		if winner.err != nil {
			winner, loser = loser, winner
		}
		if winner.err != nil || loser.task != nil || !errors.Is(loser.err, domain.ErrUploadTaskBusy) {
			t.Fatal("并发领取未产生唯一成功者", winner, loser)
		}
		assertClaimed(t, before, winner.task, winner.owner)
		if candidates, err := store.ListMultipartUploadCancellationCandidates(ctx, 1); err != nil || len(candidates) != 0 {
			t.Fatal("已有有效租约的任务仍可扫描", candidates, err)
		}
		t.Logf("任务 19 在并发竞争后只提交一份 60 秒租约，版本 %d → %d", candidate.LeaseVersion, winner.task.LeaseVersion)
	})

	// 2. 在独占连接固定时钟，检查原任务到期、租约和重试恰好到点的边界。
	t.Run("due at database time without a bound session", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		now := time.Now().UTC().Truncate(time.Second)
		execute(t, "SET timestamp = ?", now.Unix())
		defer execute(t, "SET timestamp = DEFAULT")
		resetMultipartUploadCancellationFixture(t, ctx, pool)
		execute(t, "UPDATE users SET status='DISABLED' WHERE id=7")
		execute(t, `UPDATE upload_tasks SET storage_upload_id=NULL,lease_owner='old-cleanup',lease_until=UTC_TIMESTAMP(6),
			next_attempt_at=UTC_TIMESTAMP(6),lease_version=18446744073709551614 WHERE id=19`)
		candidate, before := scan(t), read(t)
		owner := strings.Repeat("a", 64)
		claimed, err := store.ClaimMultipartUploadCancellationLease(ctx, candidate, owner)
		if err != nil || claimed == nil || claimed.LeaseVersion != math.MaxUint64 || !claimed.UpdatedAt.Equal(now) ||
			claimed.StorageUploadID != nil || !claimed.ExpiresAt.Before(now) {
			t.Fatal("到点的取消未领取最后一个可用版本", claimed, err)
		}
		assertClaimed(t, before, claimed, owner)
	})

	// 3. 旧候选、版本耗尽和回读异常都不能提交新的收尾租约。
	for _, scenario := range []struct {
		name, change    string
		changeAfterScan bool
		want            error
	}{
		{name: "retry postponed after scan", change: "UPDATE upload_tasks SET next_attempt_at=UTC_TIMESTAMP(6)+INTERVAL 1 MINUTE WHERE id=19",
			changeAfterScan: true, want: domain.ErrUploadTaskBusy},
		{name: "lease version exhausted", change: "UPDATE upload_tasks SET lease_version=18446744073709551615 WHERE id=19", want: domain.ErrUploadLeaseVersionExhausted},
		{name: "readback rejects changed raw session", change: "CREATE TRIGGER fixture_cancel_claim BEFORE UPDATE ON upload_tasks FOR EACH ROW SET NEW.storage_upload_id=RTRIM(NEW.storage_upload_id)",
			want: domain.ErrInvalidUploadTaskData},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			resetMultipartUploadCancellationFixture(t, ctx, pool)
			if !scenario.changeAfterScan {
				execute(t, scenario.change)
			}
			defer execute(t, "DROP TRIGGER IF EXISTS fixture_cancel_claim")
			candidate := scan(t)
			if scenario.changeAfterScan {
				execute(t, scenario.change)
			}
			before := read(t)
			if claimed, err := store.ClaimMultipartUploadCancellationLease(ctx, candidate, "fixture-cleanup"); claimed != nil || !errors.Is(err, scenario.want) ||
				!reflect.DeepEqual(read(t), before) {
				t.Fatal("应拒绝或回滚的领取改变了数据库", claimed, err)
			}
		})
	}
	t.Run("invalid lease owner", func(t *testing.T) {
		resetMultipartUploadCancellationFixture(t, ctx, pool)
		candidate, before := scan(t), read(t)
		for _, owner := range []string{"", strings.Repeat("a", 65), "分片处理者"} {
			if claimed, err := store.ClaimMultipartUploadCancellationLease(ctx, candidate, owner); claimed != nil || !errors.Is(err, domain.ErrInvalidUploadTaskData) ||
				!reflect.DeepEqual(read(t), before) {
				t.Fatal("接受了无效持有人或改变了数据库", owner, claimed, err)
			}
		}
	})
}
