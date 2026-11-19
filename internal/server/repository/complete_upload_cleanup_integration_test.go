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

func TestCompleteUploadExpirationIntegration(t *testing.T) {
	verifyCompleteUploadCleanup(t, domain.UploadStatusExpired, (*Store).CompleteUploadExpiration)
}

func TestCompleteUploadCancellationIntegration(t *testing.T) {
	verifyCompleteUploadCleanup(t, domain.UploadStatusCancelled, (*Store).CompleteUploadCancellation)
}

func verifyCompleteUploadCleanup(
	t *testing.T,
	terminalStatus domain.UploadStatus,
	complete func(*Store, context.Context, *domain.UploadTask) (*domain.UploadTask, error),
) {
	t.Helper()
	competingStatus := domain.UploadStatusCancelling
	if terminalStatus == domain.UploadStatusCancelled {
		competingStatus = domain.UploadStatusExpiring
	}
	type cleanupScenario struct {
		name         string
		size         uint64
		reserved     uint64
		verifying    bool
		disabled     bool
		multipart    bool
		concurrent   bool
		pastDeadline bool
		changeSQL    string
		wantError    error
	}
	scenarios := []cleanupScenario{
		{name: "release only recorded partial reservation", size: 12, reserved: 5},
		{name: "orphan a verifying object and replay", size: 12, reserved: 12, verifying: true},
		{name: "zero-byte task updates task and object"},
		{name: "disabled account and maximum lease version", size: 12, reserved: 5, disabled: true},
		{name: "concurrent attempts reuse one committed result", size: 12, reserved: 5, concurrent: true},
		{name: "reject multipart session", size: 12, reserved: 5, multipart: true, wantError: domain.ErrUploadTaskNotReady},
		{name: "reject replaced lease version", size: 12, reserved: 5,
			changeSQL: "UPDATE upload_tasks SET lease_version=lease_version+1 WHERE id=19", wantError: domain.ErrUploadTaskBusy},
		{name: "reject current processing by another holder", size: 12, reserved: 5,
			changeSQL: "UPDATE upload_tasks SET lease_owner='cleanup',lease_until=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR WHERE id=19", wantError: domain.ErrUploadTaskBusy},
		{name: "reject a pending cleanup retry", size: 12, reserved: 5,
			changeSQL: "UPDATE upload_tasks SET next_attempt_at=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR WHERE id=19", wantError: domain.ErrUploadTaskBusy},
		{name: "reject competing cleanup", size: 12, reserved: 5,
			changeSQL: "UPDATE upload_tasks SET status='" + string(competingStatus) + "' WHERE id=19", wantError: domain.ErrUploadTaskNotReady},
		{name: "reject insufficient account reservation", size: 12, reserved: 5,
			changeSQL: "UPDATE users SET reserved_storage=4 WHERE id=7", wantError: domain.ErrInvalidAccountData},
		{name: "reject a ready object", size: 12, reserved: 5,
			changeSQL: "UPDATE stored_objects SET status='READY',verified_sha256=UNHEX(REPEAT('11',32)),verified_at=UTC_TIMESTAMP(6) WHERE id=23", wantError: domain.ErrInvalidStoredObjectData},
		{name: "rollback when object still has a file reference", size: 12, reserved: 5,
			changeSQL: "UPDATE file_entries SET object_id=23 WHERE id=31", wantError: domain.ErrStoredObjectInUse},
		{name: "rollback on invalid terminal readback", size: 12, reserved: 5,
			changeSQL: "CREATE TRIGGER fixture_cleanup BEFORE UPDATE ON upload_tasks FOR EACH ROW SET NEW.last_error_code='fixture'", wantError: domain.ErrInvalidUploadTaskData},
	}
	if terminalStatus == domain.UploadStatusCancelled {
		scenarios = append(scenarios, cleanupScenario{name: "continue accepted cancellation past upload deadline", size: 12, reserved: 5, pastDeadline: true})
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 准备各自阶段的收尾输入，过期仍使用正式扫描和阶段推进。
			pool, fixture := uploadTaskDatabase(t)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}
			execute := func(statement string, args ...any) {
				t.Helper()
				if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
					t.Fatal(err)
				}
			}
			const initialUsed, otherReserved = 1024, 512
			execute("UPDATE users SET used_storage=?,reserved_storage=?,updated_at='2026-11-17 00:00:00' WHERE id=?", initialUsed, otherReserved+scenario.reserved, fixture.UserID)
			leaseVersion := uint64(3)
			if scenario.disabled {
				leaseVersion = math.MaxUint64
				execute("UPDATE users SET status='DISABLED' WHERE id=?", fixture.UserID)
			}
			status, objectStatus, mode := domain.UploadStatusUploading, domain.StoredObjectStatusStaging, "SINGLE"
			if scenario.verifying {
				status, objectStatus = domain.UploadStatusVerifying, domain.StoredObjectStatusVerifying
			}
			var uploadID, partSize, partCount any
			if scenario.multipart {
				status, mode, uploadID, partSize, partCount = domain.UploadStatusCompleting, "MULTIPART", *fixture.StorageUploadID, *fixture.PartSize, *fixture.PartCount
			}
			expiryMinutes := -1
			finishesBeforeDeadline := terminalStatus == domain.UploadStatusCancelled && !scenario.pastDeadline
			if terminalStatus == domain.UploadStatusCancelled {
				status = domain.UploadStatusCancelling
				if finishesBeforeDeadline {
					expiryMinutes = 60
				}
			}
			execute("UPDATE stored_objects SET status=?,file_size=? WHERE id=?", objectStatus, scenario.size, fixture.StagedObjectID)
			execute(`UPDATE upload_tasks SET status=?,upload_mode=?,storage_upload_id=?,part_size=?,part_count=?,
				published_object_id=NULL,resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,file_size=?,reserved_bytes=?,
				lease_owner=NULL,lease_until=NULL,lease_version=?,next_attempt_at=NULL,expires_at=TIMESTAMPADD(MINUTE,?,UTC_TIMESTAMP(6)),
				last_error_code='STORAGE_UNAVAILABLE',last_error_message='old diagnostic' WHERE id=?`,
				status, mode, uploadID, partSize, partCount, scenario.size, scenario.reserved, leaseVersion, expiryMinutes, fixture.ID)
			var pending *domain.UploadTask
			if terminalStatus == domain.UploadStatusExpired {
				candidates, err := store.ListUploadExpirationCandidates(ctx, 1)
				if err != nil || len(candidates) != 1 {
					t.Fatal("未取得待过期的任务", candidates, err)
				}
				pending, err = store.BeginUploadExpiration(ctx, candidates[0])
				if err != nil {
					t.Fatal(err)
				}
			} else {
				pending, err = store.FindUserUploadTask(ctx, fixture.UserID, fixture.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			if scenario.changeSQL != "" {
				execute(scenario.changeSQL)
			}
			type accountFacts struct {
				used, reserved uint64
				updatedAt      time.Time
			}
			read := func() (*domain.UploadTask, *domain.StoredObject, accountFacts) {
				t.Helper()
				task, err := store.FindUserUploadTask(ctx, fixture.UserID, fixture.ID)
				if err != nil {
					t.Fatal(err)
				}
				object, err := store.FindUserStoredObject(ctx, fixture.UserID, fixture.StagedObjectID)
				if err != nil {
					t.Fatal(err)
				}
				var account accountFacts
				if err := pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage,updated_at FROM users WHERE id=?", fixture.UserID).
					Scan(&account.used, &account.reserved, &account.updatedAt); err != nil {
					t.Fatal(err)
				}
				return task, object, account
			}
			beforeTask, beforeObject, beforeAccount := read()

			// 2. 执行收尾，检查两个并发调用可以观察同一提交结果。
			var result *domain.UploadTask
			if scenario.concurrent {
				type outcome struct {
					task *domain.UploadTask
					err  error
				}
				start := make(chan struct{})
				outcomes := make(chan outcome, 2)
				for range 2 {
					go func() {
						<-start
						finished, err := complete(store, ctx, pending)
						outcomes <- outcome{task: finished, err: err}
					}()
				}
				close(start)
				first, second := <-outcomes, <-outcomes
				if first.err != nil || second.err != nil || !reflect.DeepEqual(first.task, second.task) {
					t.Fatal("并发收尾没有复用同一终态结果", first, second)
				}
				result = first.task
			} else {
				result, err = complete(store, ctx, pending)
				if !errors.Is(err, scenario.wantError) {
					t.Fatal("上传收尾错误不符", result, err)
				}
			}
			afterTask, afterObject, afterAccount := read()
			if scenario.wantError != nil {
				if result != nil || !reflect.DeepEqual(beforeTask, afterTask) || !reflect.DeepEqual(beforeObject, afterObject) || beforeAccount != afterAccount {
					t.Fatal("收尾失败留下了任务、对象或账户修改", result, afterTask, afterObject, afterAccount)
				}
				return
			}

			// 3. 核对完整任务、预占返还和暂存对象回收计时。
			if result == nil || !reflect.DeepEqual(result, afterTask) || afterTask.CompletedAt == nil ||
				afterTask.CompletedAt.Before(pending.ExpiresAt) != finishesBeforeDeadline {
				t.Fatal("提交结果或收尾时间不符", result, afterTask)
			}
			beforeTask.Status, beforeTask.ReservedBytes = terminalStatus, 0
			beforeTask.CompletedAt, beforeTask.UpdatedAt = afterTask.CompletedAt, *afterTask.CompletedAt
			beforeTask.LastErrorCode, beforeTask.LastErrorMessage = nil, nil
			if !reflect.DeepEqual(beforeTask, afterTask) {
				t.Fatal("任务的终态或历史字段不符", beforeTask, afterTask)
			}
			if afterObject.UnreferencedAt == nil || afterObject.GCAfter == nil ||
				!afterObject.GCAfter.Equal(afterObject.UnreferencedAt.Add(7*24*time.Hour)) || afterObject.UnreferencedAt.Before(*afterTask.CompletedAt) {
				t.Fatal("暂存对象的回收计时错误", afterObject)
			}
			beforeObject.Status, beforeObject.UnreferencedAt, beforeObject.GCAfter = domain.StoredObjectStatusOrphaned, afterObject.UnreferencedAt, afterObject.GCAfter
			beforeObject.UpdatedAt = *afterObject.UnreferencedAt
			if !reflect.DeepEqual(beforeObject, afterObject) {
				t.Fatal("暂存对象位置或其他事实发生变化", beforeObject, afterObject)
			}
			wantAccount := beforeAccount
			wantAccount.reserved -= scenario.reserved
			if scenario.reserved > 0 {
				wantAccount.updatedAt = *afterTask.CompletedAt
			}
			if afterAccount != wantAccount {
				t.Fatal("账户没有只释放该任务的预占", afterAccount, wantAccount)
			}
			replayed, err := complete(store, ctx, pending)
			replayedTask, replayedObject, replayedAccount := read()
			if err != nil || !reflect.DeepEqual(replayed, afterTask) || !reflect.DeepEqual(replayedTask, afterTask) ||
				!reflect.DeepEqual(replayedObject, afterObject) || replayedAccount != afterAccount {
				t.Fatal("重复收尾修改了终态、计时或配额", replayed, replayedAccount, err)
			}
		})
	}
}
