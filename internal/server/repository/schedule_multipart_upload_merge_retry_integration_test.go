package repository

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
)

func TestScheduleMultipartUploadMergeRetryIntegration(t *testing.T) {
	const fileSize = domain.MaxSingleUploadSizeBytes + 1
	const session, checksum = "Fixture+A/= ", "TgdAhWK+24tgzgXB3s/jrRa3IjCWfeAfZAt+Rym0n84="
	start := time.Date(2026, 11, 22, 8, 0, 0, 0, time.UTC)
	for _, fixture := range []struct {
		name, change, beforeClaim    string
		delay, elapsed, expiry, next int
		want                         error
	}{
		{name: "wait for original lease deadline", delay: 1, next: 60},
		{name: "respect a later backoff", delay: 30, elapsed: 50, next: 80},
		{name: "cap at task expiry", delay: 30, expiry: 20, next: 20},
		{name: "old owner", change: "UPDATE upload_tasks SET lease_owner='Merge-worker' WHERE id=19", delay: 1, want: domain.ErrUploadTaskBusy},
		{name: "old version", change: "UPDATE upload_tasks SET lease_version=lease_version+1 WHERE id=19", delay: 1, want: domain.ErrUploadTaskBusy},
		{name: "different deadline", change: "UPDATE upload_tasks SET lease_until=lease_until+INTERVAL 1 SECOND WHERE id=19", delay: 1, want: domain.ErrUploadTaskBusy},
		{name: "different session", change: "UPDATE upload_tasks SET storage_upload_id='Fixture+A/=' WHERE id=19", delay: 1, want: domain.ErrUploadTaskBusy},
		{name: "lease expires exactly", elapsed: 60, delay: 1, want: domain.ErrUploadTaskBusy},
		{name: "task expires exactly", expiry: 20, elapsed: 20, delay: 1, want: domain.ErrUploadTaskNotReady},
		{name: "task progressed", change: "UPDATE upload_tasks SET status='VERIFYING' WHERE id=19", delay: 1, want: domain.ErrUploadTaskNotReady},
		{name: "reservation changed", change: "UPDATE upload_tasks SET reserved_bytes=reserved_bytes-1 WHERE id=19", delay: 1, want: domain.ErrInvalidUploadTaskData},
		{name: "attempt count exhausted", change: "UPDATE upload_tasks SET attempt_count=4294967295 WHERE id=19", delay: 1, want: domain.ErrInvalidUploadTaskData},
		{name: "lease version exhausted", beforeClaim: "UPDATE upload_tasks SET lease_version=18446744073709551614 WHERE id=19", delay: 1, want: domain.ErrUploadLeaseVersionExhausted},
		{name: "wrong account", delay: 1, want: domain.ErrUploadTaskNotFound},
		{name: "canceled transaction", delay: 1, want: context.Canceled},
		{name: "readback mismatch rolls back release", change: "CREATE TRIGGER fixture_merge_retry BEFORE UPDATE ON upload_tasks FOR EACH ROW SET NEW.next_attempt_at=NEW.updated_at", delay: 1, want: domain.ErrInvalidUploadTaskData},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 在专用库中固定连接时钟，准备已有回执并领取合并租约。
			ctx, pool, store := prepareMultipartCompletionDatabase(t)
			pool.SetMaxOpenConns(1)
			execute := func(statement string, arguments ...any) {
				t.Helper()
				if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
					t.Fatal(err)
				}
			}
			clock := func(seconds int) {
				t.Helper()
				execute("SET timestamp = ?", start.Unix()+int64(seconds))
			}
			clock(0)
			expirySeconds := fixture.expiry
			if expirySeconds == 0 {
				expirySeconds = 600
			}
			execute("UPDATE users SET reserved_storage=? WHERE id=7", fileSize)
			execute("UPDATE stored_objects SET file_size=? WHERE id=23", fileSize)
			execute(`UPDATE upload_tasks SET upload_mode='MULTIPART',status='COMPLETING',storage_upload_id=?,
				file_size=?,part_size=?,part_count=2,expected_sha256=UNHEX(REPEAT('ab',32)),reserved_bytes=?,
				published_object_id=NULL,resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,
				lease_owner=NULL,lease_until=NULL,lease_version=4,next_attempt_at=NULL,
				last_error_code=NULL,last_error_message=NULL,attempt_count=2,
				expires_at=TIMESTAMPADD(SECOND,?,UTC_TIMESTAMP(6)) WHERE id=19`, session, fileSize, fileSize-1, fileSize, expirySeconds)
			execute(`INSERT INTO upload_parts(task_id,part_number,etag,size,checksum_algorithm,checksum_value,observed_at) VALUES
				(19,1,'first',?,'SHA256',?,UTC_TIMESTAMP(6)),(19,2,'tail',1,'SHA256',?,UTC_TIMESTAMP(6))`, fileSize-1, checksum, checksum)
			if fixture.beforeClaim != "" {
				execute(fixture.beforeClaim)
			}
			scan := func(count int) []domain.MultipartUploadCompletionCandidate {
				t.Helper()
				candidates, err := store.ListMultipartUploadCompletionCandidates(ctx, 1)
				if err != nil || len(candidates) != count {
					t.Fatal("候选数量不符", candidates, err)
				}
				return candidates
			}
			claimed, err := store.ClaimMultipartUploadCompletionLease(ctx, scan(1)[0], "merge-worker")
			if err != nil || claimed.LeaseUntil == nil || !claimed.LeaseUntil.Equal(start.Add(time.Minute)) {
				t.Fatal("未取得预期的合成合并租约", claimed, err)
			}
			clock(fixture.elapsed)
			if fixture.change != "" {
				execute(fixture.change)
			}
			writeContext := ctx
			switch fixture.name {
			case "wrong account":
				claimed.UserID = 8
			case "canceled transaction":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				writeContext = canceled
			}
			type snapshot struct {
				task           *domain.UploadTask
				object         *domain.StoredObject
				parts          []models.UploadPart
				reserved, used uint64
			}
			read := func() snapshot {
				t.Helper()
				task, err := store.FindUserUploadTask(ctx, 7, 19)
				if err != nil {
					t.Fatal(err)
				}
				object, err := store.FindUserStoredObject(ctx, 7, 23)
				if err != nil {
					t.Fatal(err)
				}
				state := snapshot{task: task, object: object}
				if err := store.database.WithContext(ctx).Where("task_id = ?", 19).Order("part_number ASC").Find(&state.parts).Error; err != nil {
					t.Fatal(err)
				}
				if err := pool.QueryRowContext(ctx, "SELECT reserved_storage,used_storage FROM users WHERE id=7").Scan(&state.reserved, &state.used); err != nil {
					t.Fatal(err)
				}
				return state
			}

			// 2. 核对登记或拒绝，比较回执、对象和账号配额。
			before := read()
			result, err := store.ScheduleMultipartUploadMergeRetry(writeContext, claimed, fixture.delay)
			if !errors.Is(err, fixture.want) {
				t.Fatalf("预期错误 %v，实际 %v", fixture.want, err)
			}
			if fixture.want != nil {
				if result != nil || !reflect.DeepEqual(before, read()) {
					t.Fatal("拒绝后任务、租约、对象或回执发生变化")
				}
				return
			}
			if result == nil {
				t.Fatal("登记成功未返回任务")
			}
			before.task.LeaseOwner, before.task.LeaseUntil = nil, nil
			before.task.AttemptCount = 3
			before.task.NextAttemptAt = new(start.Add(time.Duration(fixture.next) * time.Second))
			before.task.UpdatedAt = start.Add(time.Duration(fixture.elapsed) * time.Second)
			before.task.LastErrorCode, before.task.LastErrorMessage = new("MERGE_RECHECK_REQUIRED"), new("分片合并结果需要重新核对")
			if !reflect.DeepEqual(before.task, result) || !reflect.DeepEqual(before, read()) {
				t.Fatal("登记改变了预期之外的字段，或下次核对时间不符", result)
			}

			// 3. 用受控时钟验证扫描边界、版本推进和旧租约拒绝。
			if repeated, err := store.ScheduleMultipartUploadMergeRetry(ctx, claimed, fixture.delay); repeated != nil || !errors.Is(err, domain.ErrUploadTaskBusy) {
				t.Fatal("重复登记未被拒绝", repeated, err)
			}
			clock(fixture.next - 1)
			scan(0)
			clock(fixture.next)
			if fixture.next == expirySeconds {
				scan(0)
				return
			}
			latest, err := store.ClaimMultipartUploadCompletionLease(ctx, scan(1)[0], "next-worker")
			if err != nil || latest.LeaseVersion != claimed.LeaseVersion+1 || latest.AttemptCount != result.AttemptCount {
				t.Fatal("到期后未按原任务领取新租约", latest, err)
			}
			if repeated, err := store.ScheduleMultipartUploadMergeRetry(ctx, claimed, fixture.delay); repeated != nil || !errors.Is(err, domain.ErrUploadTaskBusy) {
				t.Fatal("新租约出现后接受了旧租约写回", repeated, err)
			}
		})
	}
}
