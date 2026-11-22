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

func TestClaimMultipartUploadExpirationLeaseIntegration(t *testing.T) {
	ctx, pool, store := prepareMultipartCompletionDatabase(t)
	execute := func(t *testing.T, statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	reset := func(t *testing.T, status domain.UploadStatus) {
		t.Helper()
		resetMultipartUploadCancellationFixture(t, ctx, pool)
		execute(t, `UPDATE upload_tasks SET status=?,attempt_count=2,last_error_code='STORAGE_UNAVAILABLE',
			last_error_message='fixture previous attempt' WHERE id=19`, status)
	}
	scan := func(t *testing.T) domain.UploadExpirationCandidate {
		t.Helper()
		candidates, err := store.ListMultipartUploadExpirationCandidates(ctx, 1)
		if err != nil || len(candidates) != 1 || candidates[0].TaskID != 19 || candidates[0].UserID != 7 || candidates[0].StagedObjectID != 23 {
			t.Fatal("未取得本轮的到期候选", candidates, err)
		}
		return candidates[0]
	}
	assertNoCandidate := func(t *testing.T) {
		t.Helper()
		if candidates, err := store.ListMultipartUploadExpirationCandidates(ctx, 1); err != nil || len(candidates) != 0 {
			t.Fatal("租约或重试尚未到点时仍可扫描", candidates, err)
		}
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
		current := facts{task: task, object: object}
		if err := pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage FROM users WHERE id=7").Scan(&current.used, &current.reserved); err != nil {
			t.Fatal(err)
		}
		return current
	}
	assertClaimed := func(t *testing.T, before facts, claimed *domain.UploadTask, owner string) {
		t.Helper()
		if claimed == nil || claimed.LeaseOwner == nil || *claimed.LeaseOwner != owner || claimed.LeaseVersion != before.task.LeaseVersion+1 ||
			claimed.LeaseUntil == nil || !claimed.LeaseUntil.Equal(claimed.UpdatedAt.Add(time.Minute)) {
			t.Fatal("未返回预期的 60 秒过期收尾租约", claimed)
		}
		expected := *before.task
		if expected.Status != domain.UploadStatusExpiring {
			expected.NextAttemptAt = nil
		}
		expected.Status, expected.UpdatedAt = domain.UploadStatusExpiring, claimed.UpdatedAt
		expected.LeaseOwner, expected.LeaseUntil, expected.LeaseVersion = claimed.LeaseOwner, claimed.LeaseUntil, claimed.LeaseVersion
		before.task = &expected
		if !reflect.DeepEqual(claimed, &expected) || !reflect.DeepEqual(read(t), before) {
			t.Fatal("领取改变了原会话、分片声明、对象或账目", claimed)
		}
	}

	// 1. 两个事务争用一条首次到期的任务，推进状态和租约一同提交。
	t.Run("first expiration has one concurrent winner", func(t *testing.T) {
		reset(t, domain.UploadStatusVerifying)
		execute(t, "UPDATE stored_objects SET status='VERIFYING',storage_etag='fixture-etag',storage_version_id='fixture-version' WHERE id=23")
		execute(t, `UPDATE upload_tasks SET next_attempt_at=UTC_TIMESTAMP(6)+INTERVAL 1 MINUTE,
			lease_owner='old-verify',lease_until=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND WHERE id=19`)
		candidate, before := scan(t), read(t)
		type outcome struct {
			owner string
			task  *domain.UploadTask
			err   error
		}
		start, results := make(chan struct{}), make(chan outcome, 2)
		for _, owner := range []string{"fixture-expiry-one", "fixture-expiry-two"} {
			go func() {
				<-start
				task, err := store.ClaimMultipartUploadExpirationLease(ctx, candidate, owner)
				results <- outcome{owner: owner, task: task, err: err}
			}()
		}
		close(start)
		winner, loser := <-results, <-results
		if winner.err != nil {
			winner, loser = loser, winner
		}
		if winner.err != nil || loser.task != nil || !errors.Is(loser.err, domain.ErrUploadTaskBusy) {
			t.Fatal("并发竞争未产生唯一成功者", winner, loser)
		}
		assertClaimed(t, before, winner.task, winner.owner)
		assertNoCandidate(t)
		t.Logf("两事务仅一方推进 EXPIRING 并领取 60 秒租约，版本 %d → %d", candidate.LeaseVersion, winner.task.LeaseVersion)
	})

	// 2. 已进入 EXPIRING 的未绑定任务恰好到点后续接，保留收尾重试记录。
	t.Run("resume at deadline and preserve retry time", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		now := time.Now().UTC().Truncate(time.Second)
		execute(t, "SET timestamp = ?", now.Unix())
		defer execute(t, "SET timestamp = DEFAULT")
		reset(t, domain.UploadStatusExpiring)
		execute(t, "UPDATE users SET status='DISABLED' WHERE id=7")
		execute(t, `UPDATE upload_tasks SET storage_upload_id=NULL,lease_owner='old-expiry',lease_until=UTC_TIMESTAMP(6),
			next_attempt_at=UTC_TIMESTAMP(6),lease_version=18446744073709551614 WHERE id=19`)
		execute(t, "SET timestamp = ?", now.Add(-time.Second).Unix())
		assertNoCandidate(t)
		execute(t, "SET timestamp = ?", now.Unix())
		candidate, before := scan(t), read(t)
		owner := strings.Repeat("a", 64)
		claimed, err := store.ClaimMultipartUploadExpirationLease(ctx, candidate, owner)
		if err != nil || claimed == nil || !claimed.UpdatedAt.Equal(now) || claimed.LeaseVersion != math.MaxUint64 || claimed.StorageUploadID != nil {
			t.Fatal("到点的未绑定任务未领取最后一个可用版本", claimed, err)
		}
		assertClaimed(t, before, claimed, owner)
		assertNoCandidate(t)
		execute(t, "SET timestamp = ?", claimed.LeaseUntil.Unix())
		lastCandidate, after := scan(t), read(t)
		if claimedAgain, err := store.ClaimMultipartUploadExpirationLease(ctx, lastCandidate, owner); claimedAgain != nil ||
			!errors.Is(err, domain.ErrUploadLeaseVersionExhausted) || !reflect.DeepEqual(read(t), after) {
			t.Fatal("版本达到上限后仍可推进，或改动了记录", claimedAgain, err)
		}
	})

	// 3. 复核失效和写后核对失败时，旧阶段和原租约一起保留。
	for _, scenario := range []struct {
		name, change, owner string
		status              domain.UploadStatus
		want                error
	}{
		{"retry postponed", "UPDATE upload_tasks SET next_attempt_at=UTC_TIMESTAMP(6)+INTERVAL 1 MINUTE WHERE id=19",
			"fixture-expiry", domain.UploadStatusExpiring, domain.ErrUploadTaskNotReady},
		{"readback rejects changed session", "CREATE TRIGGER fixture_expiration_claim BEFORE UPDATE ON upload_tasks FOR EACH ROW SET NEW.storage_upload_id=RTRIM(NEW.storage_upload_id)",
			"fixture-expiry", domain.UploadStatusUploading, domain.ErrInvalidUploadTaskData},
		{"empty owner", "", "", domain.UploadStatusUploading, domain.ErrInvalidUploadTaskData},
		{"oversized owner", "", strings.Repeat("a", 65), domain.UploadStatusUploading, domain.ErrInvalidUploadTaskData},
		{"non ASCII owner", "", "分片处理者", domain.UploadStatusUploading, domain.ErrInvalidUploadTaskData},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			reset(t, scenario.status)
			candidate := scan(t)
			if scenario.change != "" {
				execute(t, scenario.change)
				defer execute(t, "DROP TRIGGER IF EXISTS fixture_expiration_claim")
			}
			before := read(t)
			if claimed, err := store.ClaimMultipartUploadExpirationLease(ctx, candidate, scenario.owner); claimed != nil ||
				!errors.Is(err, scenario.want) || !reflect.DeepEqual(read(t), before) {
				t.Fatal("拒绝或回滚未保留阶段、原会话与配额", claimed, err)
			}
		})
	}
}
