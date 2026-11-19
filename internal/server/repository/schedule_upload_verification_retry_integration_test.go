package repository

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

func TestScheduleUploadVerificationRetryIntegration(t *testing.T) {
	for _, scenario := range []struct {
		name                string
		delaySeconds        int
		changeAfterClaimSQL string
		wantError           error
		waitUntilExpiry     bool
		cancel              bool
	}{
		{name: "release current lease and let scanner reclaim when due", delaySeconds: 30},
		{name: "retry time stops at task expiry", delaySeconds: 30, waitUntilExpiry: true,
			changeAfterClaimSQL: "UPDATE upload_tasks SET expires_at=UTC_TIMESTAMP(6)+INTERVAL 15 SECOND WHERE id=19"},
		{name: "reject changed lease owner", delaySeconds: 1,
			changeAfterClaimSQL: "UPDATE upload_tasks SET lease_owner='Other-worker' WHERE id=19", wantError: domain.ErrUploadTaskBusy},
		{name: "reject changed lease version", delaySeconds: 1,
			changeAfterClaimSQL: "UPDATE upload_tasks SET lease_version=lease_version+1 WHERE id=19", wantError: domain.ErrUploadTaskBusy},
		{name: "reject expired lease", delaySeconds: 1,
			changeAfterClaimSQL: "UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6) WHERE id=19", wantError: domain.ErrUploadTaskBusy},
		{name: "reject expired task", delaySeconds: 1,
			changeAfterClaimSQL: "UPDATE upload_tasks SET expires_at=UTC_TIMESTAMP(6) WHERE id=19", wantError: domain.ErrUploadTaskBusy},
		{name: "reject non-verifying task", delaySeconds: 1,
			changeAfterClaimSQL: "UPDATE upload_tasks SET status='CANCELLING' WHERE id=19", wantError: domain.ErrUploadTaskNotReady},
		{name: "reject incomplete reservation", delaySeconds: 1,
			changeAfterClaimSQL: "UPDATE upload_tasks SET reserved_bytes=file_size-1 WHERE id=19", wantError: domain.ErrInvalidUploadTaskData},
		{name: "reject exhausted attempt count", delaySeconds: 1,
			changeAfterClaimSQL: "UPDATE upload_tasks SET attempt_count=4294967295 WHERE id=19", wantError: domain.ErrInvalidUploadTaskData},
		{name: "reject exhausted lease version", delaySeconds: 1, wantError: domain.ErrUploadLeaseVersionExhausted},
		{name: "roll back invalid readback", delaySeconds: 1,
			changeAfterClaimSQL: "CREATE TRIGGER fixture_retry BEFORE UPDATE ON upload_tasks FOR EACH ROW SET NEW.attempt_count=OLD.attempt_count", wantError: domain.ErrInvalidUploadTaskData},
		{name: "preserve cancellation", delaySeconds: 1, cancel: true, wantError: context.Canceled},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 在独占库创建待校验记录，走正式扫描和领取入口。
			pool, original := uploadTaskDatabase(t)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}
			execute := func(statement string, arguments ...any) {
				t.Helper()
				if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
					t.Fatal(err)
				}
			}
			const userID, taskID, objectID = 7, 19, 23
			const initialUsed, otherReserved = 1024, 512
			originalReserved := original.FileSize + otherReserved
			execute("UPDATE users SET used_storage=?,reserved_storage=? WHERE id=?", initialUsed, originalReserved, userID)
			execute("UPDATE stored_objects SET status='VERIFYING' WHERE id=?", objectID)
			execute(`UPDATE upload_tasks SET status='VERIFYING',upload_mode='SINGLE',storage_upload_id=NULL,part_size=NULL,
				part_count=NULL,published_object_id=NULL,resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,
				reserved_bytes=file_size,lease_owner=NULL,lease_until=NULL,lease_version=3,
				next_attempt_at=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND,attempt_count=4,last_error_code='DATABASE_UNAVAILABLE',
				last_error_message='old diagnostic',expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 DAY WHERE id=?`, taskID)
			if scenario.wantError == domain.ErrUploadLeaseVersionExhausted {
				execute("UPDATE upload_tasks SET lease_version=18446744073709551614 WHERE id=?", taskID)
			}
			list := func() []domain.UploadVerificationCandidate {
				t.Helper()
				candidates, err := store.ListUploadVerificationCandidates(ctx, 1)
				if err != nil {
					t.Fatal(err)
				}
				return candidates
			}
			candidate := list()
			if len(candidate) != 1 {
				t.Fatal("待校验任务未进入候选", candidate)
			}
			claimed, err := store.ClaimUploadVerificationLease(ctx, candidate[0], "verify-worker")
			if err != nil {
				t.Fatal(err)
			}
			if scenario.changeAfterClaimSQL != "" {
				execute(scenario.changeAfterClaimSQL)
			}
			readFacts := func() (*domain.UploadTask, *domain.StoredObject) {
				t.Helper()
				task, err := store.FindUserUploadTask(ctx, userID, taskID)
				if err != nil {
					t.Fatal(err)
				}
				object, err := store.FindUserStoredObject(ctx, userID, objectID)
				if err != nil {
					t.Fatal(err)
				}
				return task, object
			}
			before, objectBefore := readFacts()

			// 2. 用已提交的领取结果安排重试，从事务外查看写回结果。
			retryContext, cancelRetry := context.WithCancel(ctx)
			defer cancelRetry()
			if scenario.cancel {
				cancelRetry()
			}
			if scenario.wantError == nil && !scenario.waitUntilExpiry {
				for _, invalidDelay := range []int{0, 31} {
					if rejected, err := store.ScheduleUploadVerificationRetry(ctx, claimed, invalidDelay); rejected != nil || err == nil {
						t.Fatal("超出范围的等待时间被接受", invalidDelay, rejected, err)
					}
				}
			}
			updated, err := store.ScheduleUploadVerificationRetry(retryContext, claimed, scenario.delaySeconds)
			if !errors.Is(err, scenario.wantError) {
				t.Fatal("安排校验重试结果错误", updated, err)
			}
			after, objectAfter := readFacts()
			var used, reserved uint64
			if err := pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage FROM users WHERE id=?", userID).Scan(&used, &reserved); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(objectBefore, objectAfter) || used != initialUsed || reserved != originalReserved {
				t.Fatal("安排重试改变了对象或用户配额", objectAfter, used, reserved)
			}
			if scenario.wantError != nil {
				if updated != nil || !reflect.DeepEqual(before, after) {
					t.Fatal("未能安排的重试改变了任务", updated, after)
				}
				return
			}
			if updated == nil || !reflect.DeepEqual(updated, after) {
				t.Fatal("返回的重试结果与已提交任务不一致", updated, after)
			}
			wantNextAttempt := after.UpdatedAt.Add(time.Duration(scenario.delaySeconds) * time.Second)
			if scenario.waitUntilExpiry {
				wantNextAttempt = before.ExpiresAt
			}
			wantCode, wantMessage := "STORAGE_UNAVAILABLE", domain.ErrUploadStorageUnavailable.Error()
			before.LeaseOwner, before.LeaseUntil = nil, nil
			before.NextAttemptAt, before.UpdatedAt, before.AttemptCount = &wantNextAttempt, after.UpdatedAt, 5
			before.LastErrorCode, before.LastErrorMessage = &wantCode, &wantMessage
			if !reflect.DeepEqual(before, after) {
				t.Fatal("重试事实错误", before, after)
			}
			view, err := dto.NewUploadTaskData(after)
			if err != nil || view.LastError == nil || view.LastError.Code != wantCode || view.LastError.Message != wantMessage {
				t.Fatal("任务查询未交付存储暂时不可用提示", view, err)
			}

			// 3. 核对旧租约防重和到点重新领取，不等待真实时钟走完。
			if repeated, err := store.ScheduleUploadVerificationRetry(ctx, claimed, 1); repeated != nil || !errors.Is(err, domain.ErrUploadTaskBusy) {
				t.Fatal("旧租约重复安排了重试", repeated, err)
			}
			if results := list(); len(results) != 0 {
				t.Fatal("扫描提前读到了候选", results)
			}
			if scenario.waitUntilExpiry {
				execute("UPDATE upload_tasks SET expires_at=UTC_TIMESTAMP(6),next_attempt_at=UTC_TIMESTAMP(6) WHERE id=?", taskID)
				if results := list(); len(results) != 0 {
					t.Fatal("到期任务被识别为重试候选", results)
				}
				return
			}
			delayedCandidate := domain.UploadVerificationCandidate{TaskID: taskID, UserID: userID, StagedObjectID: objectID, LeaseVersion: claimed.LeaseVersion}
			if premature, err := store.ClaimUploadVerificationLease(ctx, delayedCandidate, "second-worker"); premature != nil || !errors.Is(err, domain.ErrUploadTaskNotReady) {
				t.Fatal("重新领取绕过了等待时间", premature, err)
			}
			execute("UPDATE upload_tasks SET next_attempt_at=UTC_TIMESTAMP(6) WHERE id=?", taskID)
			results := list()
			if len(results) != 1 || results[0] != delayedCandidate {
				t.Fatal("到点的重试任务未进入候选", results)
			}
			reclaimed, err := store.ClaimUploadVerificationLease(ctx, results[0], "second-worker")
			if err != nil || reclaimed.LeaseVersion != claimed.LeaseVersion+1 || reclaimed.AttemptCount != 5 {
				t.Fatal("重试领取未建立新的租约", reclaimed, err)
			}
			second, err := store.ScheduleUploadVerificationRetry(ctx, reclaimed, 1)
			if err != nil || second.AttemptCount != 6 || second.NextAttemptAt == nil || !second.NextAttemptAt.Equal(second.UpdatedAt.Add(time.Second)) {
				t.Fatal("新租约不能继续安排下一次重试", second, err)
			}
		})
	}
}
