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

func TestBeginUploadExpirationIntegration(t *testing.T) {
	for _, scenario := range []struct {
		name         string
		status       domain.UploadStatus
		multipart    bool
		disabled     bool
		leaseVersion uint64
		changeSQL    string
		clockBack    bool
		otherUser    bool
		cancel       bool
		wantError    error
	}{
		{name: "expire at exact task and lease deadline", status: domain.UploadStatusUploading, leaseVersion: 3},
		{name: "clean disabled account with maximum lease version", status: domain.UploadStatusVerifying, disabled: true, leaseVersion: math.MaxUint64},
		{name: "preserve multipart session", status: domain.UploadStatusCompleting, multipart: true, leaseVersion: 3},
		{name: "resume existing expiry without writing", status: domain.UploadStatusExpiring, leaseVersion: 5},
		{name: "reject changed lease version", status: domain.UploadStatusVerifying, leaseVersion: 3,
			changeSQL: "UPDATE upload_tasks SET lease_version=lease_version+1 WHERE id=19", wantError: domain.ErrUploadTaskBusy},
		{name: "reject changed staged object", status: domain.UploadStatusVerifying, leaseVersion: 3,
			changeSQL: "UPDATE upload_tasks SET staged_object_id=29 WHERE id=19", wantError: domain.ErrUploadTaskBusy},
		{name: "reject changed expiry", status: domain.UploadStatusVerifying, leaseVersion: 3,
			changeSQL: "UPDATE upload_tasks SET expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR WHERE id=19", wantError: domain.ErrUploadTaskBusy},
		{name: "reject already canceling", status: domain.UploadStatusUploading, leaseVersion: 3,
			changeSQL: "UPDATE upload_tasks SET status='CANCELLING' WHERE id=19", wantError: domain.ErrUploadTaskNotReady},
		{name: "reject terminal task", status: domain.UploadStatusUploading, leaseVersion: 3,
			changeSQL: "UPDATE upload_tasks SET status='FAILED',reserved_bytes=0 WHERE id=19", wantError: domain.ErrUploadTaskNotReady},
		{name: "reject task before expiry", status: domain.UploadStatusVerifying, leaseVersion: 3, clockBack: true, wantError: domain.ErrUploadTaskNotReady},
		{name: "reject renewed live lease", status: domain.UploadStatusVerifying, leaseVersion: 3,
			changeSQL: "UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6)+INTERVAL 1 MICROSECOND WHERE id=19", wantError: domain.ErrUploadTaskBusy},
		{name: "respect expiry retry schedule", status: domain.UploadStatusExpiring, leaseVersion: 3,
			changeSQL: "UPDATE upload_tasks SET next_attempt_at=UTC_TIMESTAMP(6)+INTERVAL 1 MICROSECOND WHERE id=19", wantError: domain.ErrUploadTaskNotReady},
		{name: "rollback unexpected readback", status: domain.UploadStatusVerifying, leaseVersion: 3,
			changeSQL: "CREATE TRIGGER fixture_expiration BEFORE UPDATE ON upload_tasks FOR EACH ROW SET NEW.status=OLD.status", wantError: domain.ErrInvalidUploadTaskData},
		{name: "reject cross-user task", status: domain.UploadStatusUploading, leaseVersion: 3, otherUser: true, wantError: domain.ErrUploadTaskNotFound},
		{name: "preserve caller cancellation", status: domain.UploadStatusUploading, leaseVersion: 3, cancel: true, wantError: context.Canceled},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 固定独占连接时钟，准备阶段、预占和租约恰好到期的任务。
			pool, fixture := uploadTaskDatabase(t)
			pool.SetMaxOpenConns(1)
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
			now := time.Date(2026, 11, 18, 8, 1, 0, 0, time.UTC)
			execute("SET timestamp = ?", now.Unix())
			const initialUsed, otherReserved, taskReserved = 1024, 512, 9
			execute("UPDATE users SET used_storage=?,reserved_storage=? WHERE id=?", initialUsed, otherReserved+taskReserved, fixture.UserID)
			if scenario.disabled {
				execute("UPDATE users SET status='DISABLED' WHERE id=?", fixture.UserID)
			}
			mode := "SINGLE"
			var uploadID, partSize, partCount any
			if scenario.multipart {
				mode, uploadID, partSize, partCount = "MULTIPART", *fixture.StorageUploadID, *fixture.PartSize, *fixture.PartCount
			}
			nextAttempt := now.Add(time.Hour)
			if scenario.status == domain.UploadStatusExpiring {
				nextAttempt = now
			}
			execute(`UPDATE upload_tasks SET status=?,upload_mode=?,storage_upload_id=?,part_size=?,part_count=?,
				published_object_id=NULL,resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,reserved_bytes=?,
				lease_owner='previous-worker',lease_until=UTC_TIMESTAMP(6),lease_version=?,expires_at=UTC_TIMESTAMP(6),next_attempt_at=?
				WHERE id=?`, scenario.status, mode, uploadID, partSize, partCount, taskReserved, scenario.leaseVersion, nextAttempt, fixture.ID)
			scan := func() domain.UploadExpirationCandidate {
				t.Helper()
				candidates, err := store.ListUploadExpirationCandidates(ctx, 1)
				if err != nil || len(candidates) != 1 {
					t.Fatal("未取得到期候选", candidates, err)
				}
				return candidates[0]
			}
			candidate := scan()
			if scenario.changeSQL != "" {
				execute(scenario.changeSQL)
			}
			if scenario.clockBack {
				execute("SET timestamp = ?", now.Add(-time.Second).Unix())
			}
			if scenario.otherUser {
				candidate.UserID = 8
			}
			readFacts := func() (*domain.UploadTask, *domain.StoredObject, uint64, uint64) {
				t.Helper()
				task, err := store.FindUserUploadTask(ctx, fixture.UserID, fixture.ID)
				if err != nil {
					t.Fatal(err)
				}
				object, err := store.FindUserStoredObject(ctx, fixture.UserID, fixture.StagedObjectID)
				if err != nil {
					t.Fatal(err)
				}
				var used, reserved uint64
				if err := pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage FROM users WHERE id=?", fixture.UserID).Scan(&used, &reserved); err != nil {
					t.Fatal(err)
				}
				return task, object, used, reserved
			}
			beforeTask, beforeObject, _, _ := readFacts()

			// 2. 使用扫描快照推进过期状态，或返回当前复核错误。
			attemptContext, stop := context.WithCancel(ctx)
			defer stop()
			if scenario.cancel {
				stop()
			}
			result, err := store.BeginUploadExpiration(attemptContext, candidate)
			if !errors.Is(err, scenario.wantError) {
				t.Fatal("过期推进未返回预期结果", result, err)
			}
			afterTask, afterObject, used, reserved := readFacts()
			if !reflect.DeepEqual(beforeObject, afterObject) || used != initialUsed || reserved != otherReserved+taskReserved {
				t.Fatal("推进过期阶段不应写入对象或账户配额", afterObject, used, reserved)
			}
			if scenario.wantError != nil {
				if result != nil || !reflect.DeepEqual(beforeTask, afterTask) {
					t.Fatal("复核或读回错误后任务发生修改", result, afterTask)
				}
				return
			}

			// 3. 核对完整任务、旧候选失效及已进入清理阶段的续接。
			if scenario.status != domain.UploadStatusExpiring {
				beforeTask.Status, beforeTask.UpdatedAt = domain.UploadStatusExpiring, now
				beforeTask.LeaseOwner, beforeTask.LeaseUntil, beforeTask.NextAttemptAt = nil, nil, nil
				if stale, err := store.BeginUploadExpiration(ctx, candidate); stale != nil || !errors.Is(err, domain.ErrUploadTaskBusy) {
					t.Fatal("原阶段候选仍能写入", stale, err)
				}
			}
			if !reflect.DeepEqual(result, afterTask) || !reflect.DeepEqual(beforeTask, afterTask) {
				t.Fatal("写入的过期阶段或其他任务字段不符", result, afterTask)
			}
			resumed, err := store.BeginUploadExpiration(ctx, scan())
			if err != nil || !reflect.DeepEqual(resumed, afterTask) {
				t.Fatal("复扫无法接续已有过期处理", resumed, err)
			}
			if scenario.disabled {
				if err := store.WithLoginUserTransaction(ctx, fixture.UserID, func(context.Context, *Store) error {
					t.Error("禁用账号进入登录事务")
					return nil
				}); !errors.Is(err, domain.ErrAccountDisabled) {
					t.Fatal("内部过期处理改变了登录权限", err)
				}
			}
		})
	}
}
